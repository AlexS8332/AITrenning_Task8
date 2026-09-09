package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/agents"
	"github.com/AlexS8332/AITrenning_Task8/internal/history"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task8/internal/runs"
	"github.com/AlexS8332/AITrenning_Task8/internal/tools"
)

func newTestServer(t *testing.T, dir string, fake *llmtest.Fake) (*httptest.Server, *runs.Manager) {
	t.Helper()
	manager := runs.NewManager(agents.Deps{
		Runner: agent.Runner{LLM: fake, Model: "deepseek-v4-flash"},
		Tools:  tools.NewRegistry(),
	}, history.NewStore(dir), time.Minute)
	manager.Load()
	static := fstest.MapFS{"index.html": {Data: []byte("<!DOCTYPE html><title>t</title>")}}
	srv := httptest.NewServer(New(manager, static))
	t.Cleanup(srv.Close)
	return srv, manager
}

func echoFake() *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var users []string
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser {
				users = append(users, m.Content)
			}
		}
		return llmtest.Text("помню: " + strings.Join(users, " | ")), nil
	}}
}

func postJSON(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func getJSON(t *testing.T, url string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

// drain читает поток SSE до события done и возвращает имена событий.
func drain(t *testing.T, srv *httptest.Server, turnID string) []string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/turns/" + turnID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("тип потока: %s", ct)
	}
	var events []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			name := strings.TrimPrefix(line, "event: ")
			events = append(events, name)
			if name == "done" {
				return events
			}
		}
	}
	t.Fatal("поток закончился без done")
	return nil
}

func TestAgentsAndStatic(t *testing.T) {
	srv, manager := newTestServer(t, t.TempDir(), &llmtest.Fake{})

	_, body := getJSON(t, srv.URL+"/api/agents")
	if list, _ := body["agents"].([]any); len(list) != 2 || body["historyDir"] != manager.DisplayDir() {
		t.Errorf("каталог: %+v", body)
	}

	resp, _ := http.Get(srv.URL + "/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("страница: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestValidation(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir(), &llmtest.Fake{})

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/conversations", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/conversations: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations", `{"text":"  "}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("пустое сообщение: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations", `{"text":"рысь","agent":"nope"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("неизвестный агент: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations", `{bad`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("битый JSON: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/conversations/0123456789abcdef"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("отсутствующий диалог: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations/0123456789abcdef/turns", `{"text":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("ход в отсутствующем диалоге: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/conversations/0123456789abcdef/file"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("файл отсутствующего диалога: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/conversations/0123456789abcdef/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("неизвестное действие: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/turns/missing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("отсутствующий ход: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/turns/missing/events"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("поток отсутствующего хода: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/conversations/0123456789abcdef", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("удаление отсутствующего: %d", resp.StatusCode)
	}
}

func TestDialogAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newTestServer(t, dir, echoFake())

	// Первый ход создаёт диалог.
	resp, view := postJSON(t, srv.URL+"/api/conversations", `{"agent":"chat","text":"меня зовут Алекс"}`)
	if resp.StatusCode != http.StatusAccepted || view["status"] != "running" {
		t.Fatalf("создание диалога: %d %+v", resp.StatusCode, view)
	}
	convID, _ := view["conversationId"].(string)
	turnID, _ := view["id"].(string)
	events := drain(t, srv, turnID)
	// Модель в тесте отвечает мгновенно, и журнал целиком может прийти уже в
	// снимке; отдельные log-события в таком случае необязательны.
	if events[0] != "snapshot" || events[len(events)-1] != "done" || !contains(events, "state") {
		t.Errorf("события потока: %v", events)
	}

	// Снимок хода и диалог целиком.
	_, snap := getJSON(t, srv.URL+"/api/turns/"+turnID)
	if v, _ := snap["view"].(map[string]any); v["status"] != "done" || v["reply"] != "помню: меня зовут Алекс" {
		t.Errorf("снимок хода: %+v", snap)
	}
	_, d := getJSON(t, srv.URL+"/api/conversations/"+convID)
	if msgs, _ := d["messages"].([]any); len(msgs) != 2 || d["title"] != "меня зовут Алекс" || d["active"] != nil {
		t.Errorf("диалог: %+v", d)
	}

	// Второй ход в том же диалоге.
	resp, view = postJSON(t, srv.URL+"/api/conversations/"+convID+"/turns", `{"text":"как меня зовут?"}`)
	if resp.StatusCode != http.StatusAccepted || view["history"].(float64) != 2 {
		t.Fatalf("второй ход: %d %+v", resp.StatusCode, view)
	}
	drain(t, srv, view["id"].(string))

	_, list := getJSON(t, srv.URL+"/api/conversations")
	convs, _ := list["conversations"].([]any)
	if len(convs) != 1 || convs[0].(map[string]any)["turns"].(float64) != 2 {
		t.Errorf("список: %+v", list)
	}
	_, file := getJSON(t, srv.URL+"/api/conversations/"+convID+"/file")
	if !strings.HasSuffix(file["path"].(string), convID+".json") || !strings.Contains(file["json"].(string), "как меня зовут?") {
		t.Errorf("файл: %+v", file)
	}

	// Перезапуск: новый сервер на том же каталоге продолжает диалог.
	srv.Close()
	srv2, _ := newTestServer(t, dir, echoFake())
	_, d = getJSON(t, srv2.URL+"/api/conversations/"+convID)
	if msgs, _ := d["messages"].([]any); len(msgs) != 4 {
		t.Fatalf("после перезапуска: %+v", d)
	}
	// Ход прошлого запуска в памяти нового сервера не живёт.
	if resp, _ := getJSON(t, srv2.URL+"/api/turns/"+turnID); resp.StatusCode != http.StatusNotFound {
		t.Errorf("старый ход после перезапуска: %d", resp.StatusCode)
	}
	resp, view = postJSON(t, srv2.URL+"/api/conversations/"+convID+"/turns", `{"text":"а что я говорил?"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ход после перезапуска: %d %+v", resp.StatusCode, view)
	}
	drain(t, srv2, view["id"].(string))
	_, snap = getJSON(t, srv2.URL+"/api/turns/"+view["id"].(string))
	if v, _ := snap["view"].(map[string]any); v["reply"] != "помню: меня зовут Алекс | как меня зовут? | а что я говорил?" {
		t.Errorf("после перезапуска агент не помнит: %+v", snap["view"])
	}

	req, _ := http.NewRequest(http.MethodDelete, srv2.URL+"/api/conversations/"+convID, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("удаление: %d", resp.StatusCode)
	}
	_, list = getJSON(t, srv2.URL+"/api/conversations")
	if convs, _ := list["conversations"].([]any); len(convs) != 0 {
		t.Errorf("список после удаления: %+v", list)
	}
}

func TestBusyConflict(t *testing.T) {
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("ок"), nil
	}}
	srv, _ := newTestServer(t, t.TempDir(), fake)
	_, view := postJSON(t, srv.URL+"/api/conversations", `{"agent":"chat","text":"раз"}`)
	convID := view["conversationId"].(string)

	if resp, _ := postJSON(t, srv.URL+"/api/conversations/"+convID+"/turns", `{"text":"два"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("ход во время хода: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/conversations/"+convID, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("удаление во время хода: %d", resp.StatusCode)
	}
	_, d := getJSON(t, srv.URL+"/api/conversations/"+convID)
	if d["active"] == nil || d["running"] != true {
		t.Errorf("идущий ход не виден в диалоге: %+v", d)
	}
	close(release)
	drain(t, srv, view["id"].(string))
}

func TestWriteEventAndHelpers(t *testing.T) {
	var buf bytes.Buffer
	writeEvent(&buf, "state", `{"a":1}`)
	if buf.String() != "event: state\ndata: {\"a\":1}\n\n" {
		t.Errorf("формат SSE: %q", buf.String())
	}
	if got := mustJSON(map[string]any{"x": make(chan int)}); got != "{}" {
		t.Errorf("mustJSON на несериализуемом: %q", got)
	}
	if statusOf(runs.ErrBusy) != http.StatusConflict || statusOf(runs.ErrNotFound) != http.StatusNotFound || statusOf(http.ErrBodyNotAllowed) != http.StatusBadRequest {
		t.Errorf("коды ошибок")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
