package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
)

func sampleTurn(user, reply string) (Turn, []llm.Message) {
	turn := Turn{
		ID: NewID(), Started: time.Now(), Status: TurnDone, User: user, Reply: reply,
		Totals: Totals{LLMCalls: 2, ToolCalls: 1, Usage: llm.Usage{Prompt: 100, Completion: 20, Total: 120}, Seconds: 1.5},
		Events: []agent.Event{{Seq: 1, Kind: agent.EventAgentStart, Title: "старт"}},
	}
	added := []llm.Message{
		{Role: llm.RoleUser, Content: user},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "search_wikipedia", Arguments: `{"query":"рысь"}`}}}},
		{Role: llm.RoleTool, ToolCallID: "call_1", Content: `{"results":[{"title":"Рысь"}]}`},
		{Role: llm.RoleAssistant, Content: reply},
	}
	return turn, added
}

func TestConversationAppendAndTotals(t *testing.T) {
	c := New("tools", "m")
	if c.ID == "" || !validID(c.ID) || c.Title != "" {
		t.Fatalf("новый диалог: %+v", c)
	}
	turn, added := sampleTurn("Привет! Меня зовут Алекс, расскажи про рысь", "Рысь — хищник")
	c.Append(turn, added)
	turn2, added2 := sampleTurn("а чем питается?", "зайцами")
	c.Append(turn2, added2)

	if len(c.Messages) != 8 || len(c.Turns) != 2 || c.Turns[0].Messages != 4 {
		t.Errorf("история: сообщений %d, ходов %d, у первого хода %d", len(c.Messages), len(c.Turns), c.Turns[0].Messages)
	}
	if c.Title != "Привет! Меня зовут Алекс, расскажи про рысь" {
		t.Errorf("название: %q", c.Title)
	}
	if tot := c.Totals(); tot.LLMCalls != 4 || tot.ToolCalls != 2 || tot.Usage.Total != 240 || tot.Seconds != 3 {
		t.Errorf("итоги: %+v", tot)
	}
	if c.Runes() == 0 || c.Runes() != Runes(c.Messages) {
		t.Errorf("размер истории: %d", c.Runes())
	}

	clone := c.Clone()
	clone.Messages[0].Content = "подмена"
	clone.Turns[0].Events[0].Title = "подмена"
	if c.Messages[0].Content == "подмена" || c.Turns[0].Events[0].Title == "подмена" {
		t.Errorf("Clone должен копировать глубоко")
	}
}

func TestMakeTitle(t *testing.T) {
	if got := MakeTitle("  два   слова \n и ещё "); got != "два слова и ещё" {
		t.Errorf("пробелы: %q", got)
	}
	long := strings.Repeat("слово ", 20)
	got := MakeTitle(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > titleRunes+1 || strings.HasSuffix(got, " …") {
		t.Errorf("обрезка по словам: %q", got)
	}
	if got := MakeTitle(strings.Repeat("x", 80)); len([]rune(got)) != titleRunes+1 {
		t.Errorf("обрезка без пробелов: %q", got)
	}
}

func TestCompactShortensOnlyOldToolReplies(t *testing.T) {
	long := strings.Repeat("я", 500)
	ms := []llm.Message{
		{Role: llm.RoleUser, Content: long},
		{Role: llm.RoleTool, ToolCallID: "1", Content: long},
		{Role: llm.RoleTool, ToolCallID: "2", Content: "короткий"},
		{Role: llm.RoleAssistant, Content: long},
	}
	out := Compact(ms, 100)
	if out[0].Content != long || out[3].Content != long {
		t.Errorf("сообщения пользователя и модели трогать нельзя")
	}
	if !strings.HasPrefix(out[1].Content, strings.Repeat("я", 100)+" …[сокращено") || out[1].ToolCallID != "1" {
		t.Errorf("длинный ответ инструмента не сокращён: %q", out[1].Content[:120])
	}
	if out[2].Content != "короткий" {
		t.Errorf("короткий ответ инструмента изменён: %q", out[2].Content)
	}
	if ms[1].Content != long {
		t.Errorf("Compact изменил исходный срез")
	}
	if got := Compact(ms, 0); &got[0] != &ms[0] {
		t.Errorf("keep = 0 должен вернуть историю как есть")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	store := NewStore(dir)
	if store.Dir() != dir && !filepath.IsAbs(store.Dir()) {
		t.Errorf("каталог: %q", store.Dir())
	}

	// Пустого каталога ещё нет: это не ошибка, диалогов просто нет.
	if convs, errs := store.Load(); len(convs) != 0 || len(errs) != 0 {
		t.Fatalf("загрузка из отсутствующего каталога: %v %v", convs, errs)
	}

	first := New("tools", "m")
	turn, added := sampleTurn("расскажи про рысь", "Рысь — хищник")
	first.Append(turn, added)
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	second := New("chat", "m")
	second.Updated = first.Updated.Add(time.Minute)
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}

	// Битый файл, чужой файл и файл с чужим id внутри загрузку не роняют.
	os.WriteFile(filepath.Join(dir, "0123456789abcdef.json"), []byte("{не json"), 0o644)
	os.WriteFile(filepath.Join(dir, "заметка.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "fedcba9876543210.json"), []byte(`{"id":"0000000000000000"}`), 0o644)
	if _, err := os.Stat(store.Path(first.ID) + ".tmp"); err == nil {
		t.Errorf("временный файл не должен оставаться после записи")
	}

	convs, errs := store.Load()
	if len(errs) != 2 {
		t.Errorf("ожидались две ошибки чтения, получено: %v", errs)
	}
	if len(convs) != 2 || convs[0].ID != second.ID || convs[1].ID != first.ID {
		t.Fatalf("порядок или состав диалогов: %+v", convs)
	}
	got := convs[1]
	if len(got.Messages) != 4 || got.Messages[1].ToolCalls[0].ID != "call_1" || got.Messages[2].ToolCallID != "call_1" {
		t.Errorf("вызовы инструментов не пережили запись: %+v", got.Messages)
	}
	if len(got.Turns) != 1 || got.Turns[0].Reply != "Рысь — хищник" || len(got.Turns[0].Events) != 1 {
		t.Errorf("ходы не пережили запись: %+v", got.Turns)
	}
	if got.Title != first.Title || got.AgentKey != "tools" || !got.Created.Equal(first.Created) {
		t.Errorf("метаданные: %+v", got)
	}
	if len(convs[0].Messages) != 0 || convs[0].Messages == nil || convs[0].Turns == nil {
		t.Errorf("пустой диалог должен читаться с пустыми, а не nil срезами")
	}

	raw, err := store.Raw(first.ID)
	if err != nil || !strings.Contains(string(raw), `"tool_call_id": "call_1"`) {
		t.Errorf("сырой файл: %v %s", err, raw)
	}
	if _, err := store.Raw("../etc/passwd"); err == nil {
		t.Errorf("подозрительный идентификатор должен отвергаться")
	}

	if err := store.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(first.ID); err != nil {
		t.Errorf("повторное удаление не должно быть ошибкой: %v", err)
	}
	if _, err := os.Stat(store.Path(first.ID)); !os.IsNotExist(err) {
		t.Errorf("файл не удалён")
	}
	if err := store.Save(&Conversation{ID: "bad id"}); err == nil {
		t.Errorf("диалог с некорректным id не должен записываться")
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{NewID(), "0123456789abcdef"} {
		if !validID(id) {
			t.Errorf("%q должен быть допустим", id)
		}
	}
	for _, id := range []string{"", "short", "../x", "0123456789ABCDEF", strings.Repeat("a", 40)} {
		if validID(id) {
			t.Errorf("%q не должен быть допустим", id)
		}
	}
}

// Показывать путь относительно рабочего каталога, а вне его — как есть:
// «history» вместо длинного абсолютного пути в журнале и в интерфейсе.
func TestDisplay(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore("history")
	if got := store.DisplayDir(); got != "history" {
		t.Errorf("каталог внутри рабочего: %q, ждали %q", got, "history")
	}
	want := filepath.Join("history", "abcdef0123456789.json")
	if got := store.DisplayPath("abcdef0123456789"); got != want {
		t.Errorf("файл внутри рабочего: %q, ждали %q", got, want)
	}

	// Каталог по соседству с рабочим: относительный путь начинался бы с
	// «..» и был бы длиннее и непонятнее абсолютного.
	outside := filepath.Join(filepath.Dir(cwd), "чужая-история")
	if got := Display(outside); got != outside {
		t.Errorf("каталог вне рабочего: %q, ждали %q", got, outside)
	}

	// Сам рабочий каталог — «.»: путь есть, а показывать нечего.
	if got := Display(cwd); got != "." {
		t.Errorf("рабочий каталог: %q, ждали %q", got, ".")
	}
}

// Копия пустого диалога — это пустой список сообщений, а не null. Такой
// диалог отдаётся интерфейсу сразу после создания, пока первый ход ещё
// идёт: null в JSON ронял ленту на messages.length.
func TestCloneEmptyKeepsSlices(t *testing.T) {
	clone := New("tools", "m").Clone()
	if clone.Messages == nil {
		t.Errorf("сообщения пустого диалога: nil")
	}
	if clone.Turns == nil {
		t.Errorf("ходы пустого диалога: nil")
	}

	data, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"messages":[]`) || strings.Contains(string(data), `"messages":null`) {
		t.Errorf("в JSON пустого диалога должен быть пустой список сообщений: %s", data)
	}
}
