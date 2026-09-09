// Package history — диалог как данные и его хранение между запусками.
// Диалог — это список сообщений в том виде, в каком их получает модель
// (роли user, assistant, tool, вызовы инструментов с их id), плюс ходы:
// кто что спросил, что ответил агент, во что это обошлось и журнал работы.
// Один диалог — один JSON-файл в каталоге хранилища.
package history

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
)

// Статусы хода.
const (
	TurnDone   = "done"
	TurnFailed = "failed"
)

// Сколько символов первого сообщения идёт в название диалога.
const titleRunes = 60

// Totals — счётчики хода или диалога.
type Totals struct {
	LLMCalls  int       `json:"llmCalls"`
	ToolCalls int       `json:"toolCalls"`
	Usage     llm.Usage `json:"usage"`
	Cost      llm.Cost  `json:"cost"`
	Seconds   float64   `json:"seconds"`
}

// Add складывает счётчики.
func (t Totals) Add(o Totals) Totals {
	return Totals{
		LLMCalls:  t.LLMCalls + o.LLMCalls,
		ToolCalls: t.ToolCalls + o.ToolCalls,
		Usage:     t.Usage.Add(o.Usage),
		Cost:      t.Cost.Add(o.Cost),
		Seconds:   t.Seconds + o.Seconds,
	}
}

// Turn — один ход диалога: сообщение пользователя и ответ агента.
// Messages — сколько сообщений этот ход добавил в Conversation.Messages;
// у неудачного хода ноль: в историю попадают только завершённые ходы.
type Turn struct {
	ID       string        `json:"id"`
	Started  time.Time     `json:"started"`
	Status   string        `json:"status"`
	User     string        `json:"user"`
	Reply    string        `json:"reply,omitempty"`
	Error    string        `json:"error,omitempty"`
	Messages int           `json:"messages"`
	Totals   Totals        `json:"totals"`
	Context  agent.Context `json:"context"`
	Events   []agent.Event `json:"events,omitempty"`
}

// Conversation — диалог целиком. Messages — история для модели без
// системного промпта: его добавляет агент при каждом ходе, поэтому правка
// промпта в коде действует и на старые диалоги.
type Conversation struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	AgentKey string        `json:"agent"`
	Model    string        `json:"model"`
	Created  time.Time     `json:"created"`
	Updated  time.Time     `json:"updated"`
	Messages []llm.Message `json:"messages"`
	Turns    []Turn        `json:"turns"`
}

// New создаёт пустой диалог.
func New(agentKey, model string) *Conversation {
	now := time.Now()
	return &Conversation{
		ID:       NewID(),
		AgentKey: agentKey,
		Model:    model,
		Created:  now,
		Updated:  now,
		Messages: []llm.Message{},
		Turns:    []Turn{},
	}
}

// Append записывает завершённый ход: его сообщения уходят в историю, сам
// ход — в список ходов. Название диалога берётся из первого сообщения.
func (c *Conversation) Append(turn Turn, added []llm.Message) {
	turn.Messages = len(added)
	c.Messages = append(c.Messages, added...)
	c.Turns = append(c.Turns, turn)
	c.Updated = time.Now()
	if c.Title == "" && turn.User != "" {
		c.Title = MakeTitle(turn.User)
	}
}

// Totals — сумма по всем ходам.
func (c *Conversation) Totals() Totals {
	var t Totals
	for _, turn := range c.Turns {
		t = t.Add(turn.Totals)
	}
	return t
}

// Runes — размер истории в символах: то, что модель читает на каждом ходе.
func (c *Conversation) Runes() int {
	return Runes(c.Messages)
}

// Clone — глубокая копия: диалог отдаётся наружу, пока его дописывает ход.
func (c *Conversation) Clone() *Conversation {
	out := *c
	// make + copy, а не append к nil: у пустого диалога append вернул бы
	// nil, и в JSON вместо пустого списка уходил бы null. Такой диалог
	// интерфейс запрашивает сразу после создания, пока первый ход ещё идёт.
	out.Messages = make([]llm.Message, len(c.Messages))
	copy(out.Messages, c.Messages)
	out.Turns = make([]Turn, len(c.Turns))
	for i, t := range c.Turns {
		events := make([]agent.Event, len(t.Events))
		copy(events, t.Events)
		t.Events = events
		out.Turns[i] = t
	}
	return &out
}

// MakeTitle — название диалога по первому сообщению: одна строка,
// обрезанная по словам.
func MakeTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= titleRunes {
		return text
	}
	cut := string(r[:titleRunes])
	if i := strings.LastIndex(cut, " "); i > titleRunes/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// Runes считает символы сообщений вместе с аргументами вызовов.
func Runes(ms []llm.Message) int {
	n := 0
	for _, m := range ms {
		n += len([]rune(m.Content))
		for _, c := range m.ToolCalls {
			n += len([]rune(c.Function.Arguments))
		}
	}
	return n
}

// Compact сокращает ответы инструментов прошлых ходов до keep символов.
// Файл хранит их целиком, а модели уходит короткий вариант: результаты
// поиска и разделы статей занимают тысячи символов, и без сокращения
// каждый ход тащил бы все прочитанные когда-либо статьи. Ответы модели и
// пользователя не трогаются: в них суть диалога. keep <= 0 — не сокращать.
func Compact(ms []llm.Message, keep int) []llm.Message {
	if keep <= 0 {
		return ms
	}
	out := make([]llm.Message, len(ms))
	for i, m := range ms {
		if m.Role == llm.RoleTool {
			if r := []rune(m.Content); len(r) > keep {
				m.Content = string(r[:keep]) + " …[сокращено: полный текст был в этом диалоге раньше, при необходимости вызови инструмент снова]"
			}
		}
		out[i] = m
	}
	return out
}

// NewID — идентификатор диалога или хода: 16 шестнадцатеричных знаков.
func NewID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// Store — каталог с JSON-файлами диалогов, по одному на диалог. Запись
// атомарная: во временный файл рядом и переименование, чтобы обрыв
// процесса посреди записи не оставил полуфайл вместо истории.
type Store struct {
	mu  sync.Mutex
	dir string
}

// NewStore привязывает хранилище к каталогу; сам каталог создаётся при
// первой записи.
func NewStore(dir string) *Store {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return &Store{dir: dir}
}

// Dir — каталог хранилища.
func (s *Store) Dir() string { return s.dir }

// Path — файл диалога.
func (s *Store) Path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// DisplayDir — каталог хранилища для показа человеку.
func (s *Store) DisplayDir() string { return Display(s.dir) }

// DisplayPath — файл диалога для показа человеку.
func (s *Store) DisplayPath(id string) string { return Display(s.Path(id)) }

// Display — путь в том виде, в каком его показывают в журнале запуска и в
// интерфейсе: относительно рабочего каталога, если лежит внутри него, и
// как есть в остальных случаях. Внутри хранилище держит абсолютный путь —
// он не зависит от того, сменит ли процесс каталог, — а наружу уходит
// короткий: обычно это просто «history», а заодно из интерфейса и
// скриншотов не торчит устройство чужой машины.
func Display(path string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// Save записывает диалог.
func (s *Store) Save(c *Conversation) error {
	if !validID(c.ID) {
		return fmt.Errorf("некорректный идентификатор диалога %q", c.ID)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("сериализация диалога: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("каталог истории: %w", err)
	}
	path := s.Path(c.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("запись %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("замена %s: %w", path, err)
	}
	return nil
}

// Load читает все диалоги каталога, новые первыми. Битый файл не роняет
// загрузку: он пропускается, а ошибка возвращается вместе с остальными
// диалогами, чтобы её показать.
func (s *Store) Load() ([]*Conversation, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("каталог истории: %w", err)}
	}

	var convs []*Conversation
	var problems []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		c, err := s.readFile(filepath.Join(s.dir, name))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if c.ID != strings.TrimSuffix(name, ".json") {
			problems = append(problems, fmt.Errorf("%s: внутри диалог с id %q", name, c.ID))
			continue
		}
		convs = append(convs, c)
	}
	sort.Slice(convs, func(i, j int) bool {
		return convs[i].Updated.After(convs[j].Updated)
	})
	return convs, problems
}

func (s *Store) readFile(path string) (*Conversation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Conversation
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("не разобрался: %w", err)
	}
	if !validID(c.ID) {
		return nil, fmt.Errorf("некорректный id %q", c.ID)
	}
	if c.Messages == nil {
		c.Messages = []llm.Message{}
	}
	if c.Turns == nil {
		c.Turns = []Turn{}
	}
	return &c, nil
}

// Raw — файл диалога как есть: интерфейс показывает, что лежит на диске.
func (s *Store) Raw(id string) ([]byte, error) {
	if !validID(id) {
		return nil, fmt.Errorf("некорректный идентификатор диалога %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.ReadFile(s.Path(id))
}

// Delete удаляет файл диалога. Отсутствие файла ошибкой не считается.
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return fmt.Errorf("некорректный идентификатор диалога %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.Path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// validID — идентификатор безопасен как имя файла: только hex-знаки.
func validID(id string) bool {
	if len(id) < 8 || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
