// Агент, который считает свои токены: локальный сервер с веб-интерфейсом,
// где агент-справочник по животным ведёт диалог с историей в JSON-файле, а
// рядом с каждым ходом видно, из чего сложился контекст, сколько токенов
// ушло модели и во что это обошлось. Кроме сервера есть два режима без
// интерфейса: -report прогоняет короткий, длинный и не влезающий в лимит
// диалоги и сводит их в таблицу, -probe-overflow отправляет заведомо
// слишком длинный запрос и показывает, что отвечает API.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task8/internal/agent"
	"github.com/AlexS8332/AITrenning_Task8/internal/agents"
	"github.com/AlexS8332/AITrenning_Task8/internal/history"
	"github.com/AlexS8332/AITrenning_Task8/internal/llm"
	"github.com/AlexS8332/AITrenning_Task8/internal/runs"
	"github.com/AlexS8332/AITrenning_Task8/internal/server"
	"github.com/AlexS8332/AITrenning_Task8/internal/tools"
)

// Фронтенд лежит в бинарнике: после `go build` приложение запускается одним
// файлом, без каталога с ассетами рядом.
//
//go:embed web
var webFiles embed.FS

const (
	// Общий срок одного хода: агент делает до полутора десятков запросов
	// к модели и внешним источникам.
	turnTimeout = 10 * time.Minute

	// Общий срок прогона отчёта: несколько десятков ходов подряд.
	reportTimeout = 30 * time.Minute

	// Слушаем только петлевой интерфейс: приложение локальное.
	defaultAddr = "127.0.0.1:8769"

	// Каталог с историей диалогов по умолчанию, относительно рабочего
	// каталога.
	defaultHistoryDir = "history"

	// До скольких символов сокращать ответы инструментов прошлых ходов,
	// когда история уходит модели. Файл хранит их целиком.
	defaultKeepToolRunes = 1000

	// Контекст модели в токенах. Документация DeepSeek пишет «1M», но в
	// отказе API стоит точное число: 1048576, то есть 1 Mi. Изменить его
	// через API нельзя — max_tokens ограничивает только ответ. Поэтому
	// лимит живёт здесь: агент считает токены сам и ловит переполнение до
	// отправки.
	defaultContextLimit = 1_048_576

	// Лимит для сценариев переполнения в отчёте. Он игрушечный, и в этом
	// смысл: диалог о рыси упирается в него на десятом ходе, а в настоящий
	// миллион упирался бы после нескольких тысяч ходов и за настоящие деньги.
	defaultReportLimit = 2000

	// Сколько токенов слать в пробе настоящего переполнения: чуть больше
	// лимита модели.
	defaultProbeTokens = 1_050_000
)

func main() {
	addr := flag.String("addr", defaultAddr, "адрес, на котором слушать")
	dir := flag.String("history", defaultHistoryDir, "каталог с историей диалогов (JSON, по файлу на диалог)")
	keep := flag.Int("keep-tools", defaultKeepToolRunes, "до скольких символов сокращать ответы инструментов прошлых ходов перед отправкой модели; 0 — не сокращать")
	open := flag.Bool("open", true, "открыть браузер при старте")
	limit := flag.Int("context-limit", defaultContextLimit, "свой лимит контекста в токенах; 0 — не проверять и полагаться на ответ API")
	overflow := flag.String("on-overflow", agent.OverflowFail, "что делать при переполнении: fail — не отправлять, trim — выбрасывать старые ходы, off — отправить как есть")
	report := flag.String("report", "", "прогнать опыт (короткий, длинный и не влезающий в лимит диалоги), записать отчёт в указанный файл и выйти")
	reportLimit := flag.Int("report-limit", defaultReportLimit, "лимит контекста для сценариев переполнения в отчёте")
	probe := flag.Bool("probe-overflow", false, "отправить модели заведомо слишком длинный запрос, показать ответ API и выйти")
	probeTokens := flag.Int("probe-tokens", defaultProbeTokens, "сколько примерно токенов слать в пробе переполнения")
	flag.Parse()

	switch *overflow {
	case agent.OverflowFail, agent.OverflowTrim, agent.OverflowOff:
	default:
		fail(fmt.Errorf("неизвестный режим -on-overflow=%q; допустимы fail, trim, off", *overflow))
	}

	enableUTF8Console()
	loadEnvFiles()

	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		fail(fmt.Errorf("не задан DEEPSEEK_API_KEY — задай переменную окружения или впиши ключ в .env (см. .env.example)"))
	}
	model := strings.TrimSpace(os.Getenv("DEEPSEEK_MODEL"))
	if model == "" {
		model = llm.DefaultModel
	}

	fetcher := tools.NewFetcher()
	wiki := tools.NewWikipedia(os.Getenv("WIKIPEDIA_BASE_URL"), fetcher)
	gbif := tools.NewGBIF(os.Getenv("GBIF_BASE_URL"), fetcher)
	registry := tools.NewRegistry(append(wiki.Tools(), gbif.Tools()...)...)

	deps := agents.Deps{
		Runner: agent.Runner{
			LLM:          llm.NewClient(apiKey, os.Getenv("DEEPSEEK_BASE_URL")),
			Model:        model,
			Temperature:  0,
			ContextLimit: *limit,
			OnOverflow:   *overflow,
		},
		Tools:         registry,
		KeepToolRunes: *keep,
	}

	// Опытная часть сервера не требует: тот же агент, но без интерфейса.
	// Оба режима заканчиваются выходом, чтобы прогон нельзя было спутать
	// с обычным запуском.
	if *probe {
		ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
		defer cancel()
		if err := probeOverflow(ctx, deps, *probeTokens, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if *report != "" {
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		defer cancel()
		fmt.Printf("Опыт: модель %s, лимит сценариев переполнения %d токенов\n", model, *reportLimit)
		if err := runReport(ctx, deps, model, *report, *reportLimit, os.Stdout); err != nil {
			fail(err)
		}
		return
	}

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		fail(fmt.Errorf("встроенный фронтенд не читается: %w", err))
	}

	store := history.NewStore(*dir)
	manager := runs.NewManager(deps, store, turnTimeout)
	// Восстановление контекста: всё, что лежало в каталоге до перезапуска,
	// снова доступно для продолжения.
	loaded, problems := manager.Load()
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "предупреждение: файл истории пропущен: "+p.Error())
	}
	handler := server.New(manager, static)

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fail(fmt.Errorf("не удалось занять %s: %w", *addr, err))
	}

	url := "http://" + listener.Addr().String()
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	compaction := fmt.Sprintf("старые ответы инструментов сокращаются до %d символов", *keep)
	if *keep <= 0 {
		compaction = "история уходит модели целиком"
	}
	fmt.Println("Агент с памятью между запусками")
	fmt.Println("  интерфейс:   " + url)
	fmt.Println("  модель:      " + model)
	fmt.Println("  инструменты: " + strings.Join(registry.Names(), ", "))
	fmt.Printf("  история:     %s (диалогов: %d)\n", store.DisplayDir(), loaded)
	fmt.Println("  контекст:    " + compaction)
	fmt.Println("  лимит:       " + limitLabel(*limit, *overflow))
	fmt.Println("  остановить:  Ctrl+C")

	errs := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()

	if *open {
		openBrowser(url)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	select {
	case err := <-errs:
		fail(err)
	case <-signals:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	fmt.Println("Остановлено. История диалогов осталась в " + store.DisplayDir())
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "Ошибка: "+err.Error())
	os.Exit(1)
}

// limitLabel — строка про лимит контекста для журнала запуска. Без лимита
// переполнение поймает только API, и об этом стоит сказать прямо.
func limitLabel(limit int, overflow string) string {
	if limit <= 0 {
		return "свой лимит выключен, переполнение поймает только API"
	}
	action := map[string]string{
		agent.OverflowFail: "ход не отправляется",
		agent.OverflowTrim: "выбрасываются старые ходы",
		agent.OverflowOff:  "запрос уходит как есть",
	}[overflow]
	return fmt.Sprintf("%d токенов, при переполнении — %s", limit, action)
}
