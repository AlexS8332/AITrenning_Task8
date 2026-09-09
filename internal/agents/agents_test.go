package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task8/internal/tools"
)

// fakeTools — реестр с инструментами тех же имён, что у настоящих, но
// отвечающими заготовками: агенты проверяются без сети.
func fakeTools() *tools.Registry {
	stub := func(name, out string) tools.Tool {
		return tools.Func{
			FuncName: name, FuncDescription: "подставной " + name,
			FuncParameters: json.RawMessage(`{"type":"object","properties":{}}`),
			FuncCall: func(context.Context, json.RawMessage) (string, error) {
				return out, nil
			},
		}
	}
	return tools.NewRegistry(
		stub("search_wikipedia", `{"results":[{"title":"Обыкновенная рысь"}]}`),
		stub("read_wikipedia", `{"title":"Обыкновенная рысь","text":"`+strings.Repeat("текст ", 400)+`"}`),
		stub("match_taxon", `{"found":true,"canonical_name":"Lynx lynx"}`),
		stub("taxon_tree", `{"tree":[]}`),
		stub("vernacular_names", `{"names":[]}`),
	)
}

type recorder struct{ events []agent.Event }

func (r *recorder) Log(ev agent.Event) { r.events = append(r.events, ev) }

func TestCatalog(t *testing.T) {
	if len(Catalog()) != 2 || Catalog()[0].Key != "tools" || Catalog()[1].Key != "chat" {
		t.Errorf("каталог: %+v", Infos())
	}
	if e, ok := Find("chat"); !ok || e.Title == "" || e.HasTools {
		t.Errorf("Find(chat): %+v %v", e, ok)
	}
	if e, ok := Find("tools"); !ok || !e.HasTools {
		t.Errorf("Find(tools): %+v %v", e, ok)
	}
	if _, ok := Find("team"); ok {
		t.Errorf("неизвестный тип не должен находиться")
	}
	if len(Infos()) != 2 {
		t.Errorf("Infos: %+v", Infos())
	}
}

func TestToolsAgentUsesToolsAndHistory(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		for _, name := range []string{"search_wikipedia", "read_wikipedia", "match_taxon", "taxon_tree", "vernacular_names"} {
			if !llmtest.HasTool(req, name) {
				t.Errorf("агенту не передан %s", name)
			}
		}
		switch llmtest.ToolReplies(req) {
		case 0:
			if !strings.Contains(req.Messages[0].Content, "Проверка названия") {
				t.Errorf("системный промпт: %q", req.Messages[0].Content[:60])
			}
			return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь","section":"Питание"}`), nil
		default:
			return llmtest.Text("Рысь питается зайцами."), nil
		}
	}}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}

	rec := &recorder{}
	a := NewToolsAgent(deps)
	if a.Name() != "tools" {
		t.Errorf("имя: %s", a.Name())
	}
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "расскажи про рысь"},
		{Role: llm.RoleAssistant, Content: "Рысь — хищник семейства кошачьих."},
	}
	reply, err := a.Reply(context.Background(), hist, "а чем она питается?", rec)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "Рысь питается зайцами." || len(reply.Added) != 4 {
		t.Errorf("ответ: %+v", reply)
	}
	// История ушла модели перед новым сообщением.
	first := fake.Requests[0].Messages
	if len(first) != 4 || first[1].Content != "расскажи про рысь" || first[3].Content != "а чем она питается?" {
		t.Errorf("история в запросе: %+v", first)
	}
	kinds := make([]string, 0, len(rec.events))
	for _, ev := range rec.events {
		kinds = append(kinds, ev.Kind)
	}
	got := strings.Join(kinds, " ")
	if !strings.HasPrefix(got, "agent.start prompt llm.request") || !strings.HasSuffix(got, "agent.done") {
		t.Errorf("события: %s", got)
	}
	if !strings.Contains(rec.events[0].Title, "сообщений в истории 2") {
		t.Errorf("событие старта: %q", rec.events[0].Title)
	}
}

func TestToolsAgentCompactsOldToolReplies(t *testing.T) {
	long := strings.Repeat("статья ", 1000)
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "расскажи про рысь"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "read_wikipedia", Arguments: `{"title":"Рысь"}`}}}},
		{Role: llm.RoleTool, ToolCallID: "c1", Content: long},
		{Role: llm.RoleAssistant, Content: "Рысь — хищник."},
	}
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("ок"), nil
	}}

	// С ограничением: старый ответ инструмента ушёл модели сокращённым.
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools(), KeepToolRunes: 200}
	if _, err := NewToolsAgent(deps).Reply(context.Background(), hist, "ещё?", nil); err != nil {
		t.Fatal(err)
	}
	sent := fake.Requests[0].Messages[3]
	if sent.Role != llm.RoleTool || len([]rune(sent.Content)) > 400 || !strings.Contains(sent.Content, "[сокращено") {
		t.Errorf("старый ответ инструмента не сокращён: %d символов", len([]rune(sent.Content)))
	}
	if hist[2].Content != long {
		t.Errorf("исходная история изменена")
	}

	// Без ограничения: как есть.
	deps.KeepToolRunes = 0
	if _, err := NewToolsAgent(deps).Reply(context.Background(), hist, "ещё?", nil); err != nil {
		t.Fatal(err)
	}
	if fake.Requests[1].Messages[3].Content != long {
		t.Errorf("при KeepToolRunes = 0 история должна уходить целиком")
	}
}

func TestChatHasNoToolsButKeepsHistory(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if len(req.Tools) != 0 {
			t.Errorf("у чата не должно быть инструментов: %+v", req.Tools)
		}
		if len(req.Messages) != 4 || req.Messages[1].Content != "меня зовут Алекс" {
			t.Errorf("история: %+v", req.Messages)
		}
		return llmtest.Text("Тебя зовут Алекс."), nil
	}}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}
	a := NewChat(deps)
	if a.Name() != "chat" {
		t.Errorf("имя: %s", a.Name())
	}
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "меня зовут Алекс"},
		{Role: llm.RoleAssistant, Content: "Приятно познакомиться."},
	}
	reply, err := a.Reply(context.Background(), hist, "как меня зовут?", nil)
	if err != nil || reply.Text != "Тебя зовут Алекс." || len(reply.Added) != 2 {
		t.Errorf("ответ: %+v, %v", reply, err)
	}
}

func TestAgentErrorIsLogged(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.ToolCall("search_wikipedia", `{}`), nil
	}}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}
	rec := &recorder{}
	_, err := NewChat(deps).Reply(context.Background(), nil, "q", rec)
	if err == nil {
		t.Fatal("чат с лимитом в один шаг и вызовом инструмента должен упасть")
	}
	last := rec.events[len(rec.events)-1]
	if last.Kind != agent.EventAgentError || !strings.Contains(last.Title, "лимит") {
		t.Errorf("последнее событие: %+v", last)
	}
}
