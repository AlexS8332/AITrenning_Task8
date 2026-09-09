// Агент с памятью между запусками: локальный сервер с веб-интерфейсом, в
// котором агент-справочник по животным ведёт диалог. История каждого
// диалога (сообщения в том виде, в каком их получает модель) хранится в
// JSON-файле и после перезапуска поднимается обратно, так что диалог
// продолжается с того места, где остановился.
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

	// Слушаем только петлевой интерфейс: приложение локальное.
	defaultAddr = "127.0.0.1:8769"

	// Каталог с историей диалогов по умолчанию, относительно рабочего
	// каталога.
	defaultHistoryDir = "history"

	// До скольких символов сокращать ответы инструментов прошлых ходов,
	// когда история уходит модели. Файл хранит их целиком.
	defaultKeepToolRunes = 1000

	// Контекст модели в токенах. У deepseek-v4-flash и deepseek-v4-pro это
	// 1M; изменить его через API нельзя — max_tokens ограничивает только
	// ответ. Поэтому лимит живёт здесь: агент считает токены сам и ловит
	// переполнение до отправки. Меньшее значение флага превращает опыт с
	// переполнением из миллиона токенов в четыре тысячи.
	defaultContextLimit = 1_000_000
)

func main() {
	addr := flag.String("addr", defaultAddr, "адрес, на котором слушать")
	dir := flag.String("history", defaultHistoryDir, "каталог с историей диалогов (JSON, по файлу на диалог)")
	keep := flag.Int("keep-tools", defaultKeepToolRunes, "до скольких символов сокращать ответы инструментов прошлых ходов перед отправкой модели; 0 — не сокращать")
	open := flag.Bool("open", true, "открыть браузер при старте")
	limit := flag.Int("context-limit", defaultContextLimit, "свой лимит контекста в токенах; 0 — не проверять и полагаться на ответ API")
	overflow := flag.String("on-overflow", agent.OverflowFail, "что делать при переполнении: fail — не отправлять, trim — выбрасывать старые ходы, off — отправить как есть")
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
