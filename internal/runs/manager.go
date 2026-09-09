package runs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/agents"
	"github.com/AlexS8332/AITrenning_Task8/internal/history"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
)

// Сколько завершённых ходов держим в памяти для потока событий: журнал
// каждого хода после завершения лежит в файле диалога.
const maxTurnsInMemory = 50

var (
	// ErrNotFound — диалога нет ни в памяти, ни в хранилище.
	ErrNotFound = errors.New("диалог не найден")
	// ErrBusy — в диалоге уже идёт ход; следующий можно отправить после ответа.
	ErrBusy = errors.New("агент ещё отвечает на предыдущее сообщение")
)

// Summary — диалог для списка: без сообщений и журналов.
type Summary struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	AgentKey   string         `json:"agentKey"`
	AgentTitle string         `json:"agentTitle"`
	Model      string         `json:"model"`
	Created    time.Time      `json:"created"`
	Updated    time.Time      `json:"updated"`
	Turns      int            `json:"turns"`
	Messages   int            `json:"messages"`
	Runes      int            `json:"runes"`
	Totals     history.Totals `json:"totals"`
	Running    bool           `json:"running"`
	Path       string         `json:"path"`
}

// Detail — диалог целиком: сводка, сообщения, ходы и текущий ход, если
// он идёт.
type Detail struct {
	Summary
	Messages []llm.Message  `json:"messages"`
	Turns    []history.Turn `json:"turns"`
	Active   *View          `json:"active,omitempty"`
}

// Manager хранит диалоги, запускает ходы и записывает историю.
type Manager struct {
	mu      sync.Mutex
	convs   map[string]*history.Conversation
	active  map[string]*Session // диалог → ход в работе
	turns   map[string]*Session // ход → сессия, для потока событий
	order   []string            // ходы в порядке запуска, для вытеснения
	deps    agents.Deps
	store   *history.Store
	timeout time.Duration
}

func NewManager(deps agents.Deps, store *history.Store, timeout time.Duration) *Manager {
	return &Manager{
		convs:   make(map[string]*history.Conversation),
		active:  make(map[string]*Session),
		turns:   make(map[string]*Session),
		deps:    deps,
		store:   store,
		timeout: timeout,
	}
}

// Load поднимает диалоги из хранилища. Это и есть восстановление контекста
// после перезапуска: всё, что лежит в каталоге, снова доступно для
// продолжения. Ошибки чтения отдельных файлов возвращаются списком.
func (m *Manager) Load() (int, []error) {
	convs, errs := m.store.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range convs {
		m.convs[c.ID] = c
	}
	return len(convs), errs
}

// DisplayDir — каталог хранилища для показа.
func (m *Manager) DisplayDir() string { return m.store.DisplayDir() }

// List — все диалоги, новые первыми.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Summary, 0, len(m.convs))
	for _, c := range m.convs {
		out = append(out, m.summaryLocked(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated.Equal(out[j].Updated) {
			return out[i].ID < out[j].ID
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out
}

func (m *Manager) summaryLocked(c *history.Conversation) Summary {
	title := c.AgentKey
	if e, ok := agents.Find(c.AgentKey); ok {
		title = e.Title
	}
	_, running := m.active[c.ID]
	return Summary{
		ID:         c.ID,
		Title:      c.Title,
		AgentKey:   c.AgentKey,
		AgentTitle: title,
		Model:      c.Model,
		Created:    c.Created,
		Updated:    c.Updated,
		Turns:      len(c.Turns),
		Messages:   len(c.Messages),
		Runes:      c.Runes(),
		Totals:     c.Totals(),
		Running:    running,
		Path:       m.store.DisplayPath(c.ID),
	}
}

// Get — диалог целиком.
func (m *Manager) Get(id string) (Detail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[id]
	if !ok {
		return Detail{}, false
	}
	clone := c.Clone()
	d := Detail{Summary: m.summaryLocked(c), Messages: clone.Messages, Turns: clone.Turns}
	if s, running := m.active[id]; running {
		v := s.View()
		d.Active = &v
	}
	return d, true
}

// Turn — ход по идентификатору, для потока событий.
func (m *Manager) Turn(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.turns[id]
	return s, ok
}

// Start создаёт диалог и делает в нём первый ход.
func (m *Manager) Start(agentKey, text string) (*Session, error) {
	entry, ok := agents.Find(agentKey)
	if !ok {
		return nil, fmt.Errorf("неизвестный тип агента: %q", agentKey)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("сообщение пустое")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	c := history.New(entry.Key, m.deps.Runner.Model)
	m.convs[c.ID] = c
	return m.sendLocked(c, entry, text)
}

// Send продолжает диалог: следующий ход с тем же типом агента.
func (m *Manager) Send(conversationID, text string) (*Session, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("сообщение пустое")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[conversationID]
	if !ok {
		return nil, ErrNotFound
	}
	if _, running := m.active[c.ID]; running {
		return nil, ErrBusy
	}
	entry, ok := agents.Find(c.AgentKey)
	if !ok {
		return nil, fmt.Errorf("диалог вёл агент %q, которого больше нет", c.AgentKey)
	}
	return m.sendLocked(c, entry, text)
}

// sendLocked запускает ход и уходит: HTTP-запрос не должен ждать модель.
func (m *Manager) sendLocked(c *history.Conversation, entry agents.Entry, text string) (*Session, error) {
	session := newSession(View{
		ID:             history.NewID(),
		ConversationID: c.ID,
		AgentKey:       entry.Key,
		Model:          c.Model,
		Started:        time.Now(),
		User:           text,
		History:        len(c.Messages),
	})
	m.active[c.ID] = session
	m.turns[session.view.ID] = session
	m.order = append(m.order, session.view.ID)
	m.evictLocked()

	// Агент получает копию истории на момент старта: пока ход идёт, других
	// ходов в этом диалоге нет, а копия защищает от правок по ссылке.
	hist := append([]llm.Message(nil), c.Messages...)
	go m.run(session, entry.Build(m.deps), hist, text)
	return session, nil
}

func (m *Manager) run(session *Session, a agent.Agent, hist []llm.Message, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()

	reply, err := a.Reply(ctx, hist, text, session)
	session.finish(reply.Text, reply.Stats.Context, err)

	// Ход попадает в историю до закрытия подписчиков: интерфейс по событию
	// done перечитывает диалог и должен увидеть новые сообщения. Неудачный
	// ход записывается без сообщений: история хранит только завершённые.
	added := reply.Added
	if err != nil {
		added = nil
	}
	m.mu.Lock()
	c := m.convs[session.view.ConversationID]
	var saveErr error
	if c != nil {
		c.Append(session.turn(), added)
		saveErr = m.store.Save(c)
	}
	delete(m.active, session.view.ConversationID)
	m.mu.Unlock()

	session.setSaveError(saveErr)
	session.closeSubs()
}

// Delete удаляет диалог из памяти и с диска. Пока в нём идёт ход, удалять
// нельзя: ход всё равно запишет файл заново.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.convs[id]; !ok {
		return ErrNotFound
	}
	if _, running := m.active[id]; running {
		return ErrBusy
	}
	if err := m.store.Delete(id); err != nil {
		return err
	}
	delete(m.convs, id)
	return nil
}

// Raw — файл диалога как есть.
func (m *Manager) Raw(id string) (string, []byte, error) {
	m.mu.Lock()
	_, ok := m.convs[id]
	m.mu.Unlock()
	if !ok {
		return "", nil, ErrNotFound
	}
	data, err := m.store.Raw(id)
	return m.store.DisplayPath(id), data, err
}

// evictLocked выбрасывает из памяти самые старые завершённые ходы.
func (m *Manager) evictLocked() {
	for len(m.order) > maxTurnsInMemory {
		id := m.order[0]
		s := m.turns[id]
		if s != nil && s.View().Status == StatusRunning {
			return
		}
		delete(m.turns, id)
		m.order = m.order[1:]
	}
}
