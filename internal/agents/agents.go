// Package agents — конкретные агенты приложения и их каталог. Оба агента
// ведут диалог: получают историю прошлых ходов и новое сообщение, а
// возвращают ответ и сообщения, которые добавились к истории.
package agents

import (
	"context"
	"fmt"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/history"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/tools"
)

// Deps — то, что нужно любому агенту: цикл с моделью и инструменты.
type Deps struct {
	Runner agent.Runner
	Tools  *tools.Registry
	// KeepToolRunes — до скольких символов сокращать ответы инструментов
	// прошлых ходов перед отправкой модели; 0 — отправлять целиком.
	KeepToolRunes int
}

// Info — описание типа агента для интерфейса.
type Info struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	HasTools    bool   `json:"hasTools"`
}

// Entry — тип агента в каталоге: описание и сборка.
type Entry struct {
	Info
	Build func(Deps) agent.Agent
}

// Catalog — все типы агентов в порядке показа. Ключ типа хранится в
// диалоге: при продолжении диалог ведёт тот же тип агента.
func Catalog() []Entry {
	return []Entry{
		{
			Info: Info{
				Key:   "tools",
				Title: "Агент с инструментами",
				Description: "Справочник по животным: ищет и читает статьи русской Википедии по разделам, " +
					"сверяет латинское название и строит дерево классификации по GBIF. " +
					"Факты берёт только из инструментов, а контекст диалога — из истории.",
				HasTools: true,
			},
			Build: NewToolsAgent,
		},
		{
			Info: Info{
				Key:   "chat",
				Title: "Чат без инструментов",
				Description: "Та же история диалога, но ответы из памяти модели, без проверок. " +
					"Точка отсчёта для сравнения.",
				HasTools: false,
			},
			Build: NewChat,
		},
	}
}

// Find ищет тип агента по ключу.
func Find(key string) (Entry, bool) {
	for _, e := range Catalog() {
		if e.Key == key {
			return e, true
		}
	}
	return Entry{}, false
}

// Infos — описания всех типов для интерфейса.
func Infos() []Info {
	entries := Catalog()
	infos := make([]Info, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, e.Info)
	}
	return infos
}

// Общий для обоих агентов блок о работе с историей диалога.
const dialogRules = `Ты ведёшь диалог, и вся его история у тебя перед глазами.
- Всё, что пользователь говорил раньше (как его зовут, о каком животном шла речь, что он просил), — часть контекста. Короткие вопросы вроде «а чем оно питается?» относятся к животному из предыдущих ходов.
- Не пересказывай каждый раз всё сначала: отвечай на текущий вопрос, опираясь на уже сказанное.
- Если пользователь спрашивает, что было раньше в диалоге, отвечай по истории, а не по догадкам.
Отвечай по-русски, в markdown, по существу.`

// run — общая обвязка хода: события старта и конца, сокращение истории.
func run(ctx context.Context, deps Deps, spec agent.Spec, hist []llm.Message, user string, em agent.Emitter, startTitle string) (agent.Reply, error) {
	if em == nil {
		em = agent.Nop{}
	}
	started := time.Now()
	em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventAgentStart,
		Title: fmt.Sprintf("%s: сообщений в истории %d", startTitle, len(hist))})

	reply, err := deps.Runner.Run(ctx, spec, history.Compact(hist, deps.KeepToolRunes), user, em)
	if err != nil {
		em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventAgentError, Title: err.Error(),
			Seconds: time.Since(started).Seconds()})
		return reply, err
	}
	em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventAgentDone,
		Title: fmt.Sprintf("готово: шагов %d, вызовов инструментов %d, к истории добавлено сообщений %d",
			reply.Stats.Steps, reply.Stats.ToolCalls, len(reply.Added)),
		Seconds: time.Since(started).Seconds()})
	return reply, nil
}
