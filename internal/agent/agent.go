// Package agent — контракты агента: ход диалога, события журнала и сам
// интерфейс Agent. Пакет не знает о конкретных агентах, о хранилище и об
// HTTP: его импортируют и агенты, и сервер, и история.
package agent

import (
	"context"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/tokens"
)

// Reply — итог одного хода: ответ пользователю и всё, что добавилось к
// истории за этот ход. Added хранится как есть: сообщение пользователя,
// ответы модели с вызовами инструментов, ответы инструментов и итоговый
// текст. Именно из этих сообщений складывается контекст следующего хода.
type Reply struct {
	Text  string
	Added []llm.Message
	Stats Stats
}

// Stats — счётчики одного хода.
type Stats struct {
	Steps     int
	ToolCalls int
	Usage     llm.Usage
	Cost      llm.Cost
	Context   Context
}

// Context — что занимало контекст в этом ходе. Estimate — оценка на
// старте хода с разбивкой по частям; Peak — оценка перед последним
// запросом хода: внутри хода контекст растёт с каждым ответом
// инструмента. FirstPrompt — фактические токены запроса на первом шаге,
// именно с ними сравнима оценка старта. Limit и Trimmed заполнены, когда
// работал свой лимит контекста.
type Context struct {
	Estimate    tokens.Estimate `json:"estimate"`
	Peak        int             `json:"peak"`
	FirstPrompt int             `json:"firstPrompt"`
	Limit       int             `json:"limit,omitempty"`
	// Trimmed — сколько сообщений истории выброшено, чтобы ход влез.
	Trimmed int `json:"trimmed,omitempty"`
}

// Agent — то, что ведёт диалог. History — сообщения прошлых ходов без
// системного промпта: системный промпт агент добавляет сам, чтобы правки
// в коде действовали и на старые диалоги. User — новое сообщение.
type Agent interface {
	Name() string
	Reply(ctx context.Context, history []llm.Message, user string, em Emitter) (Reply, error)
}

// Виды событий журнала.
const (
	EventAgentStart = "agent.start"
	EventAgentDone  = "agent.done"
	EventAgentError = "agent.error"
	EventLLMRequest = "llm.request"
	EventLLMReply   = "llm.response"
	EventToolCall   = "tool.call"
	EventToolResult = "tool.result"
	EventToolError  = "tool.error"
	EventNote       = "note"
	// EventPrompt — что именно получает модель на старте хода: системный
	// промпт, новое сообщение, размер истории и описания инструментов. В
	// Detail лежит JSON вида Prompt; интерфейс показывает его отдельной
	// панелью, а не в ленте.
	EventPrompt = "prompt"
)

// Prompt — содержимое события EventPrompt.
type Prompt struct {
	System string       `json:"system"`
	User   string       `json:"user"`
	Tools  []PromptTool `json:"tools"`
	// History — сколько сообщений прошлых ходов ушло модели перед новым
	// сообщением, и сколько в них символов.
	History      int `json:"history"`
	HistoryRunes int `json:"historyRunes"`
	// Estimate — оценка запроса в токенах с разбивкой по частям, Limit —
	// свой лимит контекста, если он задан.
	Estimate tokens.Estimate `json:"estimate"`
	Limit    int             `json:"limit,omitempty"`
}

// PromptTool — инструмент, доступный агенту, как он описан модели.
type PromptTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Event — запись журнала. Title — одна строка для ленты, Detail — текст
// под ней (аргументы, результат инструмента, ответ модели), Usage и Cost
// заполнены только у ответов модели.
type Event struct {
	Seq     int        `json:"seq"`
	Time    time.Time  `json:"time"`
	Agent   string     `json:"agent"`
	Kind    string     `json:"kind"`
	Step    int        `json:"step,omitempty"`
	Title   string     `json:"title"`
	Detail  string     `json:"detail,omitempty"`
	Usage   *llm.Usage `json:"usage,omitempty"`
	Cost    *llm.Cost  `json:"cost,omitempty"`
	Seconds float64    `json:"seconds,omitempty"`
	// Tokens — сколько токенов насчитала оценка перед запросом и сколько
	// их оказалось по ответу модели. У запроса заполнена только оценка, у
	// ответа — оба числа и расхождение.
	Tokens *tokens.Tokens `json:"tokens,omitempty"`
}

// Emitter принимает события журнала. Реализация обязана быть безопасной
// для вызова из нескольких горутин.
type Emitter interface {
	Log(Event)
}

// Nop — эмиттер, который всё отбрасывает. Для тестов и вызовов, где
// журнал не нужен.
type Nop struct{}

func (Nop) Log(Event) {}
