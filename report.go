package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/agents"
	"github.com/AlexS8332/AITrenning_Task8/internal/history"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/tokens"
)

// Здесь живёт опытная часть задания: прогнать короткий диалог, длинный и
// такой, который в контекст не влезает, и свести числа в одну таблицу.
// Сервер для этого не нужен — нужен тот же агент, что и в интерфейсе.

// scenario — один прогон: чем отвечать, что спрашивать и с каким лимитом.
type scenario struct {
	Name     string
	Note     string
	Agent    string
	Limit    int
	Overflow string
	Messages []string
}

// turnRow — строка таблицы: один ход одного сценария.
type turnRow struct {
	N          int
	User       string
	HistoryMsg int
	Estimate   tokens.Estimate
	FirstReal  int
	Usage      llm.Usage
	Cost       llm.Cost
	Cumulative float64
	Trimmed    int
	Seconds    float64
	Err        string
}

// result — итог сценария.
type result struct {
	scenario scenario
	rows     []turnRow
	total    llm.Usage
	cost     float64
	seconds  float64
}

// Вопросы подобраны так, чтобы диалог был связным: каждый следующий
// опирается на предыдущие ответы, и модель не может отвечать в отрыве от
// истории. Короткий сценарий — начало того же разговора, длинный — он же,
// доведённый до заметной истории.
var dialogQuestions = []string{
	"Привет! Меня зовут Алекс. Расскажи коротко про рысь.",
	"А чем она питается?",
	"Где она обитает?",
	"Она опасна для человека?",
	"Чем рысь отличается от каракала?",
	"А от манула?",
	"Сколько рысь живёт в природе?",
	"Как выглядят её котята?",
	"Она хорошо плавает?",
	"Кто её естественные враги?",
	"Как она охотится зимой?",
	"Напомни, как меня зовут и с чего мы начали разговор.",
}

func scenarios(limit int) []scenario {
	return []scenario{
		{
			Name:     "короткий диалог",
			Note:     "три хода: истории почти нет, платим за вопрос и ответ",
			Agent:    "chat",
			Messages: dialogQuestions[:3],
		},
		{
			Name:     "длинный диалог",
			Note:     "двенадцать ходов того же разговора: история растёт и уходит модели заново каждый ход",
			Agent:    "chat",
			Messages: dialogQuestions,
		},
		{
			Name:     "переполнение, режим fail",
			Note:     fmt.Sprintf("тот же диалог при лимите %d токенов: ход не отправляется, как только история перестаёт влезать", limit),
			Agent:    "chat",
			Limit:    limit,
			Overflow: agent.OverflowFail,
			Messages: dialogQuestions,
		},
		{
			Name:     "переполнение, режим trim",
			Note:     fmt.Sprintf("тот же лимит %d, но старые ходы выбрасываются: диалог продолжается ценой памяти", limit),
			Agent:    "chat",
			Limit:    limit,
			Overflow: agent.OverflowTrim,
			Messages: dialogQuestions,
		},
	}
}

// runReport прогоняет сценарии и пишет отчёт в markdown. Каждый сценарий
// начинается с пустой истории: диалоги независимы, сравнивать их можно.
func runReport(ctx context.Context, deps agents.Deps, model, path string, limit int, log io.Writer) error {
	results := make([]result, 0, 4)
	for _, sc := range scenarios(limit) {
		fmt.Fprintf(log, "\n%s: %s\n", sc.Name, sc.Note)
		res, err := runScenario(ctx, deps, sc, log)
		if err != nil {
			return err
		}
		results = append(results, res)
	}

	var b strings.Builder
	writeReport(&b, model, limit, results)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("запись отчёта: %w", err)
	}
	fmt.Fprintf(log, "\nОтчёт записан: %s\n", history.Display(path))
	return nil
}

// runScenario ведёт диалог до конца или до первой ошибки. Ошибка ход не
// прерывает молча: она попадает в таблицу, потому что «что ломается при
// переполнении» — это и есть предмет опыта.
func runScenario(ctx context.Context, deps agents.Deps, sc scenario, log io.Writer) (result, error) {
	entry, ok := agents.Find(sc.Agent)
	if !ok {
		return result{}, fmt.Errorf("неизвестный агент %q", sc.Agent)
	}
	deps.Runner.ContextLimit = sc.Limit
	deps.Runner.OnOverflow = sc.Overflow
	a := entry.Build(deps)

	res := result{scenario: sc}
	var hist []llm.Message
	var cumulative float64

	for i, msg := range sc.Messages {
		started := time.Now()
		reply, err := a.Reply(ctx, hist, msg, nil)
		elapsed := time.Since(started).Seconds()

		row := turnRow{
			N: i + 1, User: msg, HistoryMsg: len(hist),
			Estimate:  reply.Stats.Context.Estimate,
			FirstReal: reply.Stats.Context.FirstPrompt,
			Usage:     reply.Stats.Usage,
			Cost:      reply.Stats.Cost,
			Trimmed:   reply.Stats.Context.Trimmed,
			Seconds:   elapsed,
		}
		if err != nil {
			row.Err = err.Error()
			res.rows = append(res.rows, row)
			fmt.Fprintf(log, "  ход %2d: %s\n", row.N, err)
			// Ход не состоялся — история не растёт, следующий упрётся в то
			// же самое. Дальше идти незачем: результат уже виден.
			break
		}

		if reply.Stats.Cost.Known {
			cumulative += reply.Stats.Cost.USD
		}
		row.Cumulative = cumulative
		res.rows = append(res.rows, row)
		res.total = res.total.Add(reply.Stats.Usage)
		res.seconds += elapsed
		res.cost = cumulative
		hist = append(hist, reply.Added...)

		fmt.Fprintf(log, "  ход %2d: история %3d сообщ., ≈%6d оценка, %6d факт, %5d выход, %s%s\n",
			row.N, row.HistoryMsg, row.Estimate.Total, row.FirstReal, row.Usage.Completion,
			usd(row.Cost), trimNote(row.Trimmed))
	}
	return res, nil
}

func trimNote(trimmed int) string {
	if trimmed == 0 {
		return ""
	}
	return fmt.Sprintf(" (выброшено сообщений: %d)", trimmed)
}

// probeQuestion — вопрос, которым заканчивается проба переполнения.
const probeQuestion = "Сколько всего ты помнишь?"

// overflowHistory набирает историю примерно на target токенов из
// повторяющихся ходов. Шаг цикла — вес обоих сообщений: на удвоенном весе
// длинного набиралась ровно половина заказанного, потому что вопрос
// короткий.
func overflowHistory(target int) ([]llm.Message, tokens.Estimate) {
	question := llm.Message{Role: llm.RoleUser, Content: "Расскажи ещё про рысь."}
	answer := llm.Message{Role: llm.RoleAssistant,
		Content: strings.Repeat("Рысь — хищник семейства кошачьих, обитающий в таёжных лесах. ", 400)}
	step := int(tokens.Default.Message(question) + tokens.Default.Message(answer))

	var hist []llm.Message
	for est := 0; est < target; est += step {
		hist = append(hist, question, answer)
	}
	return hist, tokens.Of("", nil, hist, probeQuestion)
}

// probeOverflow отправляет заведомо слишком длинный запрос и показывает,
// что ответит API. Свой лимит здесь выключен намеренно: смысл пробы в
// том, чтобы услышать настоящий отказ модели, а не своё сообщение.
// Отвергнутый запрос не тарифицируется, поэтому проба бесплатна.
func probeOverflow(ctx context.Context, deps agents.Deps, target int, log io.Writer) error {
	hist, est := overflowHistory(target)
	fmt.Fprintf(log, "Проба переполнения: %d сообщений, оценка ≈%d токенов (лимит модели %d).\n",
		len(hist), est.Total, modelContextLimit)

	// Запрос, который модель примет, стоит денег и ничего не доказывает.
	// Проба имеет смысл, только если он заведомо не влезает.
	if est.Total < modelContextLimit {
		return fmt.Errorf("набрано только ≈%d токенов: такой запрос модель примет и он будет оплачен; увеличь -probe-tokens", est.Total)
	}

	deps.Runner.ContextLimit = 0
	deps.Runner.OnOverflow = agent.OverflowOff
	entry, _ := agents.Find("chat")
	started := time.Now()
	_, err := entry.Build(deps).Reply(ctx, hist, probeQuestion, nil)
	elapsed := time.Since(started)

	if err == nil {
		fmt.Fprintf(log, "Модель ответила: запрос уложился в контекст. Увеличь -probe-tokens.\n")
		return nil
	}
	fmt.Fprintf(log, "Ответ API через %.1f с:\n\n%s\n", elapsed.Seconds(), err)
	return nil
}

func writeReport(b *strings.Builder, model string, limit int, results []result) {
	fmt.Fprintf(b, "# Токены и стоимость по мере диалога\n\n")
	fmt.Fprintf(b, "Модель `%s`, температура 0, агент без инструментов: в опыте важен рост истории, ", model)
	fmt.Fprintf(b, "а не работа с источниками. Дата прогона: %s.\n\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(b, "Оценка считается до отправки (`internal/tokens`), факт берётся из `usage` ответа API. ")
	fmt.Fprintf(b, "«Факт» в таблице — расход первого запроса хода: только он сравним с оценкой, ")
	fmt.Fprintf(b, "дальше внутри хода к контексту добавляются ответы инструментов.\n\n")

	for _, res := range results {
		fmt.Fprintf(b, "## %s\n\n%s\n\n", res.scenario.Name, res.scenario.Note)
		if res.scenario.Limit > 0 {
			fmt.Fprintf(b, "Свой лимит контекста: %d токенов, при переполнении — `%s`.\n\n",
				res.scenario.Limit, res.scenario.Overflow)
		}
		fmt.Fprintf(b, "| ход | вопрос | история, сообщ. | оценка | факт | ошибка | из кэша | выход | ход стоил | всего |\n")
		fmt.Fprintf(b, "|---|---|---|---|---|---|---|---|---|---|\n")
		for _, r := range res.rows {
			if r.Err != "" {
				fmt.Fprintf(b, "| %d | %s | %d | ≈%d | — | — | — | — | — | **ход не состоялся** |\n",
					r.N, shorten(r.User), r.HistoryMsg, r.Estimate.Total)
				continue
			}
			fmt.Fprintf(b, "| %d%s | %s | %d | ≈%d | %d | %s | %d | %d | %s | %s |\n",
				r.N, trimMark(r.Trimmed), shorten(r.User), r.HistoryMsg,
				r.Estimate.Total, r.FirstReal, errPct(r.Estimate.Total, r.FirstReal),
				r.Usage.CacheHit, r.Usage.Completion, usd(r.Cost), usdValue(r.Cumulative))
		}
		fmt.Fprintf(b, "\nИтого: %d ходов, %d токенов на вход, %d на выход, %s, %.1f с.\n\n",
			len(res.rows), res.total.Prompt, res.total.Completion, usdValue(res.cost), res.seconds)
		for _, r := range res.rows {
			if r.Err != "" {
				fmt.Fprintf(b, "Ход %d не состоялся:\n\n```\n%s\n```\n\n", r.N, r.Err)
			}
		}
	}

	fmt.Fprintf(b, "## Точность оценки\n\n")
	fmt.Fprintf(b, "| сценарий | ходов | средняя ошибка оценки |\n|---|---|---|\n")
	for _, res := range results {
		var sum float64
		var n int
		for _, r := range res.rows {
			if r.Err == "" && r.FirstReal > 0 {
				sum += absPct(r.Estimate.Total, r.FirstReal)
				n++
			}
		}
		if n == 0 {
			fmt.Fprintf(b, "| %s | 0 | — |\n", res.scenario.Name)
			continue
		}
		fmt.Fprintf(b, "| %s | %d | %.1f %% |\n", res.scenario.Name, n, sum/float64(n))
	}
	fmt.Fprintf(b, "\nЛимит контекста модели — %d токенов, и через API он не меняется: ", modelContextLimit)
	fmt.Fprintf(b, "`max_tokens` ограничивает только ответ. Поэтому лимит в таблицах выше — свой, ")
	fmt.Fprintf(b, "агентский: он позволяет поставить тот же опыт на тысячах токенов вместо миллиона.\n")
}

// modelContextLimit — контекст deepseek-v4-flash и deepseek-v4-pro.
// Документация округляет до «1M», API в отказе называет точное число.
const modelContextLimit = 1_048_576

func shorten(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:39]) + "…"
}

func trimMark(trimmed int) string {
	if trimmed == 0 {
		return ""
	}
	return fmt.Sprintf(" ✂%d", trimmed)
}

func errPct(estimate, actual int) string {
	if actual == 0 {
		return "—"
	}
	pct := float64(estimate-actual) / float64(actual) * 100
	return fmt.Sprintf("%+.0f %%", pct)
}

func absPct(estimate, actual int) float64 {
	pct := float64(estimate-actual) / float64(actual) * 100
	if pct < 0 {
		return -pct
	}
	return pct
}

func usd(c llm.Cost) string {
	if !c.Known {
		return "—"
	}
	return usdValue(c.USD)
}

func usdValue(v float64) string {
	if v >= 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.6f", v)
}
