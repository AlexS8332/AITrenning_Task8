package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/agents"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task8/internal/tools"
)

// growingFake — подставная модель: отвечает длинно и честно сообщает
// расход, посчитанный по длине запроса. Этого хватает, чтобы в отчёте
// росли и токены, и стоимость.
func growingFake() *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var runes int
		for _, m := range req.Messages {
			runes += len([]rune(m.Content))
		}
		resp := llmtest.Text(strings.Repeat("Рысь — хищник семейства кошачьих. ", 12))
		prompt := runes / 2
		resp.Usage = llm.Usage{Prompt: prompt, Completion: 120, Total: prompt + 120, CacheHit: prompt / 3}
		return resp, nil
	}}
}

func reportDeps(fake *llmtest.Fake) agents.Deps {
	return agents.Deps{
		Runner: agent.Runner{LLM: fake, Model: "deepseek-v4-flash"},
		Tools:  tools.NewRegistry(),
	}
}

func TestReportComparesScenarios(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.md")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Лимит подобран так, чтобы длинный диалог в него не влез: сценарии
	// переполнения должны действительно переполниться.
	if err := runReport(ctx, reportDeps(growingFake()), "deepseek-v4-flash", path, 900, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	for _, want := range []string{
		"короткий диалог", "длинный диалог",
		"переполнение, режим fail", "переполнение, режим trim",
		"Точность оценки", "ход не состоялся",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в отчёте нет %q", want)
		}
	}
	if !strings.Contains(text, "запрос не влезает в контекст") {
		t.Errorf("в отчёте нет текста ошибки переполнения:\n%s", text)
	}
}

func TestReportGrowsWithHistory(t *testing.T) {
	// Суть опыта: тот же агент, те же вопросы, но чем длиннее диалог, тем
	// дороже ход. Если это перестанет быть правдой, отчёт врёт.
	sc := scenario{Name: "рост", Agent: "chat", Messages: dialogQuestions[:6]}
	res, err := runScenario(context.Background(), reportDeps(growingFake()), sc, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.rows) != 6 {
		t.Fatalf("ходов в результате: %d", len(res.rows))
	}
	for i := 1; i < len(res.rows); i++ {
		if res.rows[i].Estimate.Total <= res.rows[i-1].Estimate.Total {
			t.Errorf("ход %d: контекст не вырос (%d → %d)", i+1,
				res.rows[i-1].Estimate.Total, res.rows[i].Estimate.Total)
		}
		if res.rows[i].Cumulative < res.rows[i-1].Cumulative {
			t.Errorf("ход %d: накопленная стоимость уменьшилась", i+1)
		}
	}
	// История растёт ровно на сообщения хода: вопрос и ответ.
	if res.rows[5].HistoryMsg != 10 {
		t.Errorf("сообщений в истории перед шестым ходом: %d", res.rows[5].HistoryMsg)
	}
}

func TestReportOverflowStopsDialog(t *testing.T) {
	// В режиме fail диалог обрывается на первом же ходе, который не влез,
	// и в таблице остаётся строка с ошибкой, а не молчание.
	sc := scenario{Name: "переполнение", Agent: "chat", Limit: 700,
		Overflow: agent.OverflowFail, Messages: dialogQuestions}
	res, err := runScenario(context.Background(), reportDeps(growingFake()), sc, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	last := res.rows[len(res.rows)-1]
	if last.Err == "" {
		t.Fatalf("диалог должен был упереться в лимит: %d ходов без ошибки", len(res.rows))
	}
	if len(res.rows) == len(dialogQuestions) {
		t.Errorf("после переполнения продолжать нечего, а прошли все ходы")
	}
	if !strings.Contains(last.Err, "700") {
		t.Errorf("в ошибке должен быть лимит: %s", last.Err)
	}
}

func TestLimitLabel(t *testing.T) {
	if got := limitLabel(0, agent.OverflowFail); !strings.Contains(got, "только API") {
		t.Errorf("без лимита: %q", got)
	}
	if got := limitLabel(4000, agent.OverflowTrim); !strings.Contains(got, "старые ходы") || !strings.Contains(got, "4000") {
		t.Errorf("с лимитом: %q", got)
	}
}

func TestOverflowHistoryReachesTarget(t *testing.T) {
	// Проба имеет смысл, только если набранная история действительно
	// больше заказанного: на первом заходе шаг цикла считался по удвоенному
	// весу длинного сообщения, и набиралась ровно половина.
	for _, target := range []int{10_000, 200_000, 1_050_000} {
		_, est := overflowHistory(target)
		if est.Total < target {
			t.Errorf("для %d набрано только ≈%d токенов", target, est.Total)
		}
	}
}
