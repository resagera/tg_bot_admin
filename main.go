package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	stateNone = iota
	stateWaitAddAdmin
	stateWaitAddCommandName
	stateWaitAddCommandBody
	stateWaitAddCommandScope
	stateWaitEditCommandSelect
	stateWaitEditCommandBody
	stateWaitAddPageID
	stateWaitAddPageContent
	stateWaitEditPageID
	stateWaitEditPageContent
)

type Config struct {
	Token         string  `json:"token"`
	BaseURL       string  `json:"base_url"`
	ListenAddr    string  `json:"listen_addr"`
	DataDir       string  `json:"data_dir"`
	InitialAdmins []int64 `json:"initial_admins"`
}

type Command struct {
	Name    string `json:"name"`
	OwnerID int64  `json:"owner_id"`
	Public  bool   `json:"public"`
	Expr    string `json:"expr"`
}

type UserState struct {
	Step      int    `json:"step"`
	TempName  string `json:"temp_name"`
	TempScope string `json:"temp_scope"`
	TempPage  string `json:"temp_page"`
}

type Store struct {
	mu       sync.RWMutex
	Admins   map[int64]bool                `json:"admins"`
	Commands map[string]Command            `json:"commands"`
	States   map[int64]UserState           `json:"states"`
	Pages    map[int64]map[string]struct{} `json:"pages"`
	path     string
}

func NewStore(path string, initialAdmins []int64) (*Store, error) {
	s := &Store{
		Admins:   make(map[int64]bool),
		Commands: make(map[string]Command),
		States:   make(map[int64]UserState),
		Pages:    make(map[int64]map[string]struct{}),
		path:     path,
	}

	for _, id := range initialAdmins {
		s.Admins[id] = true
	}

	if _, err := os.Stat(path); err == nil {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var loaded Store
		if err := json.Unmarshal(b, &loaded); err != nil {
			return nil, err
		}
		s.Admins = loaded.Admins
		s.Commands = loaded.Commands
		s.States = loaded.States
		s.Pages = loaded.Pages
	}

	for _, id := range initialAdmins {
		s.Admins[id] = true
	}

	return s, s.Save()
}

func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tmp := struct {
		Admins   map[int64]bool                `json:"admins"`
		Commands map[string]Command            `json:"commands"`
		States   map[int64]UserState           `json:"states"`
		Pages    map[int64]map[string]struct{} `json:"pages"`
	}{
		Admins:   s.Admins,
		Commands: s.Commands,
		States:   s.States,
		Pages:    s.Pages,
	}

	b, err := json.MarshalIndent(tmp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0644)
}

func (s *Store) IsAdmin(userID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Admins[userID]
}

func (s *Store) AddAdmin(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Admins[userID] = true
	return nil
}

func (s *Store) SetState(userID int64, st UserState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.States[userID] = st
	return nil
}

func (s *Store) GetState(userID int64) UserState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.States[userID]
}

func (s *Store) ResetState(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.States, userID)
	return nil
}

func (s *Store) AddCommand(cmd Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Commands[cmd.Name] = cmd
	return nil
}

func (s *Store) GetCommand(name string) (Command, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cmd, ok := s.Commands[name]
	return cmd, ok
}

func (s *Store) ListCommandsFor(userID int64) []Command {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Command
	for _, c := range s.Commands {
		if c.Public || c.OwnerID == userID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) ListOwnCommands(userID int64) []Command {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Command
	for _, c := range s.Commands {
		if c.OwnerID == userID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) AddPage(userID int64, pageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Pages[userID] == nil {
		s.Pages[userID] = make(map[string]struct{})
	}
	s.Pages[userID][pageID] = struct{}{}
	return nil
}

func (s *Store) ListPages(userID int64) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []string
	for p := range s.Pages[userID] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

var allowedExecutables = map[string]bool{
	"echo":   true,
	"date":   true,
	"uname":  true,
	"uptime": true,
	"whoami": true,
	"pwd":    true,
	"ls":     true,
	"cat":    true,
	"grep":   true,
}

func main() {
	cfg, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "pages"), 0755); err != nil {
		log.Fatalf("mkdir pages dir: %v", err)
	}

	store, err := NewStore(filepath.Join(cfg.DataDir, "store.json"), cfg.InitialAdmins)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}

	go runHTTP(cfg)

	bot, err := tgbotapi.NewBotAPI(cfg.Token)
	if err != nil {
		log.Fatalf("telegram init: %v", err)
	}

	_, err = bot.Request(tgbotapi.DeleteWebhookConfig{
		DropPendingUpdates: true,
	})
	if err != nil {
		log.Fatalf("delete webhook: %v", err)
	}
	bot.Debug = false

	log.Printf("authorized as @%s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30

	updates := bot.GetUpdatesChan(u)

	for upd := range updates {
		if upd.Message == nil {
			log.Printf("skip non-message update")
			continue
		}
		log.Printf(
			"message: user_id=%d chat_id=%d text=%q",
			upd.Message.From.ID,
			upd.Message.Chat.ID,
			upd.Message.Text,
		)
		if err := handleMessage(bot, store, cfg, upd.Message); err != nil {
			log.Printf("handle message error: %v", err)
			msg := tgbotapi.NewMessage(upd.Message.Chat.ID, "Ошибка: "+err.Error())
			_, _ = bot.Send(msg)
		}
		if err := store.Save(); err != nil {
			log.Printf("store save error: %v", err)
		}
	}
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	if err != nil {
		return cfg, err
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8483"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	return cfg, nil
}

func runHTTP(cfg Config) {
	root := filepath.Join(cfg.DataDir, "pages")
	mux := http.NewServeMux()

	fsHandler := http.FileServer(http.Dir(root))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean(r.URL.Path)
		full := filepath.Join(root, clean)
		if _, err := os.Stat(full); errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		fsHandler.ServeHTTP(w, r)
	}))

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("http server listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
}

func handleMessage(bot *tgbotapi.BotAPI, store *Store, cfg Config, m *tgbotapi.Message) error {
	userID := m.From.ID
	chatID := m.Chat.ID

	if !store.IsAdmin(userID) {
		msg := tgbotapi.NewMessage(chatID, "Доступ запрещён.")
		_, _ = bot.Send(msg)
		return nil
	}

	if m.IsCommand() {
		switch m.Command() {
		case "start":
			return sendMainMenu(bot, chatID)
		case "cancel":
			_ = store.ResetState(userID)
			return sendText(bot, chatID, "Действие отменено.", mainKeyboard())
		}
	}

	if m.Text != "" {
		switch m.Text {
		case "Добавить админа":
			_ = store.SetState(userID, UserState{Step: stateWaitAddAdmin})
			return sendText(bot, chatID, "Отправь Telegram user ID нового админа.", cancelKeyboard())

		case "Добавить команду":
			_ = store.SetState(userID, UserState{Step: stateWaitAddCommandName})
			return sendText(bot, chatID, "Введи имя команды.", cancelKeyboard())

		case "Изменить команду":
			cmds := store.ListOwnCommands(userID)
			if len(cmds) == 0 {
				return sendText(bot, chatID, "У тебя нет своих команд для редактирования.", mainKeyboard())
			}
			var b strings.Builder
			b.WriteString("Твои команды:\n")
			for _, c := range cmds {
				scope := "private"
				if c.Public {
					scope = "public"
				}
				b.WriteString(fmt.Sprintf("- %s (%s)\n", c.Name, scope))
			}
			_ = store.SetState(userID, UserState{Step: stateWaitEditCommandSelect})
			return sendText(bot, chatID, b.String()+"\nВведи имя команды для редактирования.", cancelKeyboard())

		case "Добавить страницу":
			_ = store.SetState(userID, UserState{Step: stateWaitAddPageID})
			return sendText(bot, chatID, "Введи номер страницы, например: 1", cancelKeyboard())

		case "Изменить страницу":
			pages := store.ListPages(userID)
			if len(pages) == 0 {
				return sendText(bot, chatID, "У тебя пока нет страниц.", mainKeyboard())
			}
			return startEditPage(bot, store, userID, chatID, pages)

		case "Команды":
			return sendCommandsMenu(bot, store, userID, chatID)
		}
	}

	state := store.GetState(userID)

	switch state.Step {
	case stateWaitAddAdmin:
		newID, err := strconv.ParseInt(strings.TrimSpace(m.Text), 10, 64)
		if err != nil {
			return sendText(bot, chatID, "Неверный user ID.", cancelKeyboard())
		}
		if err := store.AddAdmin(newID); err != nil {
			return err
		}
		_ = store.ResetState(userID)
		return sendText(bot, chatID, fmt.Sprintf("Админ %d добавлен.", newID), mainKeyboard())

	case stateWaitAddCommandName:
		name := strings.TrimSpace(m.Text)
		if name == "" {
			return sendText(bot, chatID, "Имя команды не должно быть пустым.", cancelKeyboard())
		}
		if _, exists := store.GetCommand(name); exists {
			return sendText(bot, chatID, "Команда с таким именем уже существует.", cancelKeyboard())
		}
		_ = store.SetState(userID, UserState{
			Step:     stateWaitAddCommandBody,
			TempName: name,
		})
		return sendText(bot, chatID, "Введи выражение.\nРазрешены только whitelist-команды, например:\ncat file.txt | grep text", cancelKeyboard())

	case stateWaitAddCommandBody:
		expr := strings.TrimSpace(m.Text)
		if err := validatePipeline(expr); err != nil {
			return sendText(bot, chatID, "Недопустимое выражение: "+err.Error(), cancelKeyboard())
		}
		_ = store.SetState(userID, UserState{
			Step:      stateWaitAddCommandScope,
			TempName:  state.TempName,
			TempScope: expr,
		})
		return sendText(bot, chatID, "Введи scope: public или private", cancelKeyboard())

	case stateWaitAddCommandScope:
		scope := strings.ToLower(strings.TrimSpace(m.Text))
		if scope != "public" && scope != "private" {
			return sendText(bot, chatID, "Нужно указать public или private", cancelKeyboard())
		}
		cmd := Command{
			Name:    state.TempName,
			OwnerID: userID,
			Public:  scope == "public",
			Expr:    state.TempScope,
		}
		if err := store.AddCommand(cmd); err != nil {
			return err
		}
		_ = store.ResetState(userID)
		return sendText(bot, chatID, fmt.Sprintf("Команда %q добавлена.", cmd.Name), mainKeyboard())

	case stateWaitEditCommandSelect:
		name := strings.TrimSpace(m.Text)
		cmd, ok := store.GetCommand(name)
		if !ok || cmd.OwnerID != userID {
			return sendText(bot, chatID, "Команда не найдена среди твоих.", cancelKeyboard())
		}
		_ = store.SetState(userID, UserState{
			Step:     stateWaitEditCommandBody,
			TempName: name,
		})
		return sendText(bot, chatID, "Введи новое выражение команды.", cancelKeyboard())

	case stateWaitEditCommandBody:
		expr := strings.TrimSpace(m.Text)
		if err := validatePipeline(expr); err != nil {
			return sendText(bot, chatID, "Недопустимое выражение: "+err.Error(), cancelKeyboard())
		}
		cmd, ok := store.GetCommand(state.TempName)
		if !ok || cmd.OwnerID != userID {
			return sendText(bot, chatID, "Команда не найдена.", mainKeyboard())
		}
		cmd.Expr = expr
		if err := store.AddCommand(cmd); err != nil {
			return err
		}
		_ = store.ResetState(userID)
		return sendText(bot, chatID, fmt.Sprintf("Команда %q обновлена.", cmd.Name), mainKeyboard())

	case stateWaitAddPageID:
		pageID := strings.TrimSpace(m.Text)
		if pageID == "" || strings.Contains(pageID, "/") || strings.Contains(pageID, "..") {
			return sendText(bot, chatID, "Некорректный номер страницы.", cancelKeyboard())
		}
		_ = store.SetState(userID, UserState{
			Step:     stateWaitAddPageContent,
			TempPage: pageID,
		})
		return sendText(bot, chatID, "Отправь HTML-содержимое страницы.", cancelKeyboard())

	case stateWaitAddPageContent:
		if err := savePage(cfg, store, userID, state.TempPage, m.Text); err != nil {
			return err
		}
		url := buildPageURL(cfg.BaseURL, userID, state.TempPage)
		_ = store.ResetState(userID)
		return sendText(bot, chatID, "Страница сохранена:\n"+url, mainKeyboard())

	case stateWaitEditPageID:
		pageID := strings.TrimSpace(m.Text)
		found := false
		for _, p := range store.ListPages(userID) {
			if p == pageID {
				found = true
				break
			}
		}
		if !found {
			return sendText(bot, chatID, "Страница не найдена.", cancelKeyboard())
		}
		_ = store.SetState(userID, UserState{
			Step:     stateWaitEditPageContent,
			TempPage: pageID,
		})
		return sendText(bot, chatID, "Отправь новое HTML-содержимое страницы.", cancelKeyboard())

	case stateWaitEditPageContent:
		if err := savePage(cfg, store, userID, state.TempPage, m.Text); err != nil {
			return err
		}
		url := buildPageURL(cfg.BaseURL, userID, state.TempPage)
		_ = store.ResetState(userID)
		return sendText(bot, chatID, "Страница обновлена:\n"+url, mainKeyboard())
	}

	if strings.HasPrefix(m.Text, "/run ") {
		name := strings.TrimSpace(strings.TrimPrefix(m.Text, "/run "))
		return runNamedCommand(bot, store, userID, chatID, name)
	}

	return sendMainMenu(bot, chatID)
}

func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64) error {
	return sendText(bot, chatID, "Выбери действие:", mainKeyboard())
}

func mainKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Добавить админа"),
			tgbotapi.NewKeyboardButton("Добавить команду"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Изменить команду"),
			tgbotapi.NewKeyboardButton("Добавить страницу"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Изменить страницу"),
			tgbotapi.NewKeyboardButton("Команды"),
		),
	)
}

func cancelKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("/cancel"),
		),
	)
}

func sendText(bot *tgbotapi.BotAPI, chatID int64, text string, kb tgbotapi.ReplyKeyboardMarkup) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	_, err := bot.Send(msg)
	return err
}

func sendCommandsMenu(bot *tgbotapi.BotAPI, store *Store, userID, chatID int64) error {
	cmds := store.ListCommandsFor(userID)
	if len(cmds) == 0 {
		return sendText(bot, chatID, "Команд пока нет.", mainKeyboard())
	}

	var b strings.Builder
	b.WriteString("Список команд:\n\n")
	for _, c := range cmds {
		scope := "private"
		if c.Public {
			scope = "public"
		}
		b.WriteString(fmt.Sprintf("%s [%s]\n/run %s\n\n", c.Name, scope, c.Name))
	}
	return sendText(bot, chatID, b.String(), mainKeyboard())
}

func startEditPage(bot *tgbotapi.BotAPI, store *Store, userID, chatID int64, pages []string) error {
	var b strings.Builder
	b.WriteString("Твои страницы:\n")
	for _, p := range pages {
		b.WriteString("- " + p + "\n")
	}
	b.WriteString("\nВведи номер страницы для редактирования.")
	_ = store.SetState(userID, UserState{Step: stateWaitEditPageID})
	return sendText(bot, chatID, b.String(), cancelKeyboard())
}

func savePage(cfg Config, store *Store, userID int64, pageID, content string) error {
	dir := filepath.Join(cfg.DataDir, "pages", strconv.FormatInt(userID, 10), pageID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	file := filepath.Join(dir, "index.html")
	if err := os.WriteFile(file, []byte(content), 0644); err != nil {
		return err
	}
	if err := store.AddPage(userID, pageID); err != nil {
		return err
	}
	return nil
}

func buildPageURL(base string, userID int64, pageID string) string {
	return strings.TrimRight(base, "/") + "/" + strconv.FormatInt(userID, 10) + "/" + pageID + "/"
}

func runNamedCommand(bot *tgbotapi.BotAPI, store *Store, userID, chatID int64, name string) error {
	cmd, ok := store.GetCommand(name)
	if !ok {
		return sendText(bot, chatID, "Команда не найдена.", mainKeyboard())
	}
	if !cmd.Public && cmd.OwnerID != userID {
		return sendText(bot, chatID, "Нет доступа к этой команде.", mainKeyboard())
	}

	out, err := runPipeline(cmd.Expr, 15*time.Second)
	if err != nil {
		return sendBigText(bot, chatID, "Ошибка выполнения:\n"+err.Error()+"\n\n"+out)
	}
	if strings.TrimSpace(out) == "" {
		out = "(пустой вывод)"
	}
	return sendBigText(bot, chatID, out)
}

func sendBigText(bot *tgbotapi.BotAPI, chatID int64, text string) error {
	const limit = 3500
	for len(text) > 0 {
		part := text
		if len(part) > limit {
			part = part[:limit]
		}
		msg := tgbotapi.NewMessage(chatID, part)
		msg.ReplyMarkup = mainKeyboard()
		if _, err := bot.Send(msg); err != nil {
			return err
		}
		if len(text) <= limit {
			break
		}
		text = text[limit:]
	}
	return nil
}

func validatePipeline(expr string) error {
	parts := splitPipe(expr)
	if len(parts) == 0 {
		return errors.New("пустая команда")
	}
	for _, p := range parts {
		args, err := splitArgs(p)
		if err != nil {
			return err
		}
		if len(args) == 0 {
			return errors.New("пустой сегмент pipe")
		}
		if !allowedExecutables[args[0]] {
			return fmt.Errorf("команда %q не разрешена", args[0])
		}
		for _, a := range args[1:] {
			if strings.ContainsAny(a, "&;`$><") {
				return fmt.Errorf("опасный символ в аргументе %q", a)
			}
		}
	}
	return nil
}

func splitPipe(expr string) []string {
	raw := strings.Split(expr, "|")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inQuote := false
	var quote byte

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if inQuote {
			if ch == quote {
				inQuote = false
				continue
			}
			cur.WriteByte(ch)
			continue
		}

		switch ch {
		case '\'', '"':
			inQuote = true
			quote = ch
		case ' ', '\t', '\n':
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(ch)
		}
	}

	if inQuote {
		return nil, errors.New("незакрытая кавычка")
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args, nil
}

func runPipeline(expr string, timeout time.Duration) (string, error) {
	if err := validatePipeline(expr); err != nil {
		return "", err
	}

	parts := splitPipe(expr)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var cmds []*exec.Cmd
	for _, p := range parts {
		args, err := splitArgs(p)
		if err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmds = append(cmds, cmd)
	}

	var stderr bytes.Buffer
	for _, c := range cmds {
		c.Stderr = &stderr
	}

	for i := 0; i < len(cmds)-1; i++ {
		r, w := ioPipe()
		cmds[i].Stdout = w
		cmds[i+1].Stdin = r
	}

	var stdout bytes.Buffer
	cmds[len(cmds)-1].Stdout = &stdout

	for _, c := range cmds {
		if err := c.Start(); err != nil {
			return "", err
		}
	}

	for i := 0; i < len(cmds)-1; i++ {
		if pw, ok := cmds[i].Stdout.(*pipeWriter); ok {
			defer pw.Close()
		}
		if pr, ok := cmds[i+1].Stdin.(*pipeReader); ok {
			defer pr.Close()
		}
	}

	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			out := stdout.String()
			if stderr.Len() > 0 {
				out += "\n" + stderr.String()
			}
			return out, err
		}
	}

	out := stdout.String()
	if stderr.Len() > 0 {
		out += "\n" + stderr.String()
	}
	return out, nil
}

type pipeReader struct{ *os.File }
type pipeWriter struct{ *os.File }

func ioPipe() (*pipeReader, *pipeWriter) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	return &pipeReader{r}, &pipeWriter{w}
}
