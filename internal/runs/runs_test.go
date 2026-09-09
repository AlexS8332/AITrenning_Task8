package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/agents"
	"github.com/AlexS8332/AITrenning_Task8/internal/history"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task8/internal/tools"
)

func stubTools() *tools.Registry {
	stub := func(name string) tools.Tool {
		return tools.Func{
			FuncName: name, FuncDescription: name,
			FuncParameters: json.RawMessage(`{"type":"object","properties":{}}`),
			FuncCall: func(context.Context, json.RawMessage) (string, error) {
				return `{"title":"Обыкновенная рысь","text":"` + strings.Repeat("рысь ", 300) + `"}`, nil
			},
		}
	}
	return tools.NewRegistry(stub("search_wikipedia"), stub("read_wikipedia"), stub("match_taxon"), stub("taxon_tree"), stub("vernacular_names"))
}

// echoModel — модель, которая на первом шаге хода зовёт инструмент, а на
// втором отвечает текстом, повторяя, сколько сообщений истории видит.
func echoModel() *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == llm.RoleUser && llmtest.HasTool(req, "read_wikipedia") {
			return llmtest.ToolCall("read_wikipedia", `{"title":"Рысь"}`), nil
		}
		var users []string
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser {
				users = append(users, m.Content)
			}
		}
		return llmtest.Text(fmt.Sprintf("сообщений: %02d", len(req.Messages)) + "; вопросы: " + strings.Join(users, " | ")), nil
	}}
}

func newManager(t *testing.T, dir string, fake *llmtest.Fake, keep int) *Manager {
	t.Helper()
	deps := agents.Deps{
		Runner:        agent.Runner{LLM: fake, Model: "m"},
		Tools:         stubTools(),
		KeepToolRunes: keep,
	}
	return NewManager(deps, history.NewStore(dir), 5*time.Second)
}

// wait дожидается конца хода через подписку и возвращает итоговый вид.
func wait(t *testing.T, s *Session) View {
	t.Helper()
	_, updates, unsubscribe := s.Subscribe()
	defer unsubscribe()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				return s.View()
			}
		case <-deadline:
			t.Fatal("ход не завершился")
		}
	}
}

func TestConversationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	// Первый запуск: диалог из двух ходов.
	fake := echoModel()
	m1 := newManager(t, dir, fake, 0)
	if n, errs := m1.Load(); n != 0 || len(errs) != 0 {
		t.Fatalf("пустое хранилище: %d %v", n, errs)
	}
	s1, err := m1.Start("tools", "  меня зовут Алекс, расскажи про рысь ")
	if err != nil {
		t.Fatal(err)
	}
	v1 := wait(t, s1)
	if v1.Status != StatusDone || v1.History != 0 || v1.SaveError != "" {
		t.Fatalf("первый ход: %+v", v1)
	}
	if !strings.HasPrefix(v1.Reply, "сообщений: 04") {
		t.Errorf("первый ответ: %q", v1.Reply)
	}
	if v1.Totals.LLMCalls != 2 || v1.Totals.ToolCalls != 1 || v1.Events == 0 {
		t.Errorf("итоги первого хода: %+v", v1)
	}

	convID := v1.ConversationID
	s2, err := m1.Send(convID, "а чем она питается?")
	if err != nil {
		t.Fatal(err)
	}
	v2 := wait(t, s2)
	// История: user, assistant(tool_calls), tool, assistant = 4 сообщения.
	if v2.History != 4 || !strings.Contains(v2.Reply, "вопросы: меня зовут Алекс, расскажи про рысь | а чем она питается?") {
		t.Errorf("второй ход не видит историю: %+v", v2)
	}

	list := m1.List()
	if len(list) != 1 || list[0].Turns != 2 || list[0].Messages != 8 || list[0].Title != "меня зовут Алекс, расскажи про рысь" || list[0].Running {
		t.Errorf("список: %+v", list)
	}
	if list[0].Totals.LLMCalls != 4 || list[0].Runes == 0 || list[0].AgentTitle == "" {
		t.Errorf("сводка: %+v", list[0])
	}
	path, raw, err := m1.Raw(convID)
	if err != nil || !strings.HasSuffix(path, convID+".json") || !strings.Contains(string(raw), `"role": "tool"`) {
		t.Errorf("файл диалога: %v %s", err, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл диалога не записан: %v", err)
	}

	// «Перезапуск»: новый менеджер и новая модель на том же каталоге.
	fake2 := echoModel()
	m2 := newManager(t, dir, fake2, 0)
	n, errs := m2.Load()
	if n != 1 || len(errs) != 0 {
		t.Fatalf("после перезапуска: диалогов %d, ошибки %v", n, errs)
	}
	d, ok := m2.Get(convID)
	if !ok || len(d.Messages) != 8 || len(d.Turns) != 2 || d.Turns[1].User != "а чем она питается?" || d.Active != nil {
		t.Fatalf("диалог после перезапуска: %+v", d.Summary)
	}
	if len(d.Turns[0].Events) == 0 || d.Turns[0].Events[0].Kind != agent.EventAgentStart {
		t.Errorf("журнал хода не восстановлен: %+v", d.Turns[0].Events)
	}

	s3, err := m2.Send(convID, "как меня зовут?")
	if err != nil {
		t.Fatal(err)
	}
	v3 := wait(t, s3)
	if v3.History != 8 || !strings.Contains(v3.Reply, "меня зовут Алекс, расскажи про рысь | а чем она питается? | как меня зовут?") {
		t.Errorf("после перезапуска агент не помнит диалог: %+v", v3)
	}
	// Модель второго запуска получила всю историю первого, включая вызовы
	// инструментов с их идентификаторами.
	first := fake2.Requests[0].Messages
	if len(first) != 10 || first[0].Role != llm.RoleSystem || first[2].ToolCalls == nil || first[3].ToolCallID != first[2].ToolCalls[0].ID {
		t.Errorf("история в запросе после перезапуска: %d сообщений", len(first))
	}

	d, _ = m2.Get(convID)
	if len(d.Messages) != 12 || len(d.Turns) != 3 || d.Updated.Before(d.Created) {
		t.Errorf("диалог после третьего хода: %+v", d.Summary)
	}

	// И третий запуск видит все три хода.
	m3 := newManager(t, dir, echoModel(), 0)
	m3.Load()
	if d, ok := m3.Get(convID); !ok || len(d.Turns) != 3 {
		t.Errorf("третий запуск: %+v", d.Summary)
	}
}

func TestCompactionAppliesToOldTurnsOnly(t *testing.T) {
	fake := echoModel()
	m := newManager(t, t.TempDir(), fake, 100)
	s, _ := m.Start("tools", "рысь")
	wait(t, s)
	s2, _ := m.Send(s.View().ConversationID, "ещё")
	wait(t, s2)

	// На втором ходе старый ответ инструмента ушёл сокращённым, а в файле
	// он целый.
	req := fake.Requests[2].Messages
	if req[3].Role != llm.RoleTool || !strings.Contains(req[3].Content, "[сокращено") {
		t.Errorf("старый ответ инструмента не сокращён: %q", req[3].Content[:50])
	}
	d, _ := m.Get(s.View().ConversationID)
	if strings.Contains(d.Messages[2].Content, "[сокращено") || len([]rune(d.Messages[2].Content)) < 1000 {
		t.Errorf("в истории должен лежать полный текст")
	}
}

func TestBusyNotFoundAndDelete(t *testing.T) {
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("ок"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)

	if _, err := m.Start("nope", "x"); err == nil {
		t.Errorf("неизвестный агент должен отвергаться")
	}
	if _, err := m.Start("chat", "  "); err == nil {
		t.Errorf("пустое сообщение должно отвергаться")
	}
	if _, err := m.Send("0123456789abcdef", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Send в несуществующий диалог: %v", err)
	}

	s, err := m.Start("chat", "привет")
	if err != nil {
		t.Fatal(err)
	}
	id := s.View().ConversationID
	if _, err := m.Send(id, "ещё"); !errors.Is(err, ErrBusy) {
		t.Errorf("второй ход во время первого: %v", err)
	}
	if err := m.Delete(id); !errors.Is(err, ErrBusy) {
		t.Errorf("удаление во время хода: %v", err)
	}
	d, _ := m.Get(id)
	if d.Active == nil || d.Active.Status != StatusRunning || !d.Running {
		t.Errorf("идущий ход должен быть виден: %+v", d.Summary)
	}
	if got, ok := m.Turn(s.View().ID); !ok || got != s {
		t.Errorf("ход по идентификатору не находится")
	}

	close(release)
	wait(t, s)

	if _, err := m.Send(id, "ещё"); err != nil {
		t.Fatal(err)
	}
	wait(t, mustActive(t, m, id))
	if d, _ = m.Get(id); d.Active != nil || len(d.Turns) != 2 {
		t.Errorf("после второго хода: %+v", d.Summary)
	}

	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(id); ok {
		t.Errorf("диалог должен исчезнуть из памяти")
	}
	if _, _, err := m.Raw(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("файл удалённого диалога: %v", err)
	}
	if err := m.Delete(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторное удаление: %v", err)
	}
	if len(m.List()) != 0 {
		t.Errorf("список после удаления: %+v", m.List())
	}
}

func mustActive(t *testing.T, m *Manager, id string) *Session {
	t.Helper()
	d, ok := m.Get(id)
	if !ok || d.Active == nil {
		t.Fatalf("ход в диалоге %s не идёт", id)
	}
	s, _ := m.Turn(d.Active.ID)
	return s
}

func TestFailedTurnKeepsHistoryIntact(t *testing.T) {
	calls := 0
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		calls++
		if calls == 2 {
			return llm.Response{}, errors.New("сеть упала")
		}
		return llmtest.Text("ок"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)
	s, _ := m.Start("chat", "раз")
	wait(t, s)
	s2, _ := m.Send(s.View().ConversationID, "два")
	v := wait(t, s2)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "сеть упала") {
		t.Fatalf("неудачный ход: %+v", v)
	}
	d, _ := m.Get(s.View().ConversationID)
	if len(d.Messages) != 2 || len(d.Turns) != 2 || d.Turns[1].Status != history.TurnFailed || d.Turns[1].Messages != 0 {
		t.Errorf("после неудачного хода: сообщений %d, ходов %+v", len(d.Messages), d.Turns)
	}
	// Следующий ход идёт по чистой истории: неудачное сообщение в неё не
	// попало.
	s3, _ := m.Send(s.View().ConversationID, "три")
	wait(t, s3)
	last := fake.Requests[2].Messages
	if len(last) != 4 || last[3].Content != "три" {
		t.Errorf("история после неудачного хода: %+v", last)
	}
}

func TestSubscribeAfterCloseAndEviction(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("ок"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)
	s, _ := m.Start("chat", "раз")
	wait(t, s)

	snap, ch, unsubscribe := s.Subscribe()
	unsubscribe()
	if _, ok := <-ch; ok {
		t.Errorf("подписка на завершённый ход должна закрываться сразу")
	}
	if snap.View.Status != StatusDone || len(snap.Events) == 0 {
		t.Errorf("снимок завершённого хода: %+v", snap.View)
	}

	// Вытеснение: больше maxTurnsInMemory ходов, старые уходят из памяти.
	id := s.View().ConversationID
	for i := 0; i < maxTurnsInMemory+5; i++ {
		s2, err := m.Send(id, "ещё")
		if err != nil {
			t.Fatal(err)
		}
		wait(t, s2)
	}
	if _, ok := m.Turn(s.View().ID); ok {
		t.Errorf("первый ход должен быть вытеснен из памяти")
	}
	if d, _ := m.Get(id); len(d.Turns) != maxTurnsInMemory+6 {
		t.Errorf("в истории должны остаться все ходы: %d", len(d.Turns))
	}
}

// Диалог, в котором идёт первый ход, интерфейс запрашивает сразу после
// создания: сообщений в нём ещё нет. В JSON должен уйти пустой список, а
// не null — на null лента падала на messages.length.
func TestGetFreshConversation(t *testing.T) {
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("готово"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)

	session, err := m.Start("chat", "привет")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := m.Get(session.View().ConversationID)
	if !ok {
		t.Fatal("диалог не найден сразу после создания")
	}
	if d.Messages == nil || d.Turns == nil {
		t.Errorf("сообщения %v, ходы %v", d.Messages, d.Turns)
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"messages":[]`) {
		t.Errorf("в JSON идущего диалога должен быть пустой список сообщений: %s", data)
	}

	close(release)
	wait(t, session)
}
