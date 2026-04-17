package bot

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"piratesbot/internal/api"
)

type State struct {
	IsRunning bool
	TurnNo    int
	ArenaLock sync.RWMutex
	Arena     *api.PlayerResponse
}

type Bot struct {
	client *api.Client
	state  *State

	stopCh chan struct{}
	mu     sync.Mutex
	logCh  chan string

	// Strategy state between turns.
	currentAxis     string
	buildTarget     []int
	pendingRelocate []int
}

func (b *Bot) dumpTurn(arena *api.PlayerResponse, cmd api.PlayerCommand) {
	// Сохраняем в папке logs текущей директории запущеного процесса
	os.MkdirAll("logs", 0755)

	filename := fmt.Sprintf("logs/turn_%d.json", arena.TurnNo)

	data := map[string]interface{}{
		"received_arena": arena,
		"sent_command":   cmd,
	}

	bytes, _ := json.MarshalIndent(data, "", "  ")
	os.WriteFile(filename, bytes, 0644)
}

func NewBot(client *api.Client) *Bot {
	return &Bot{
		client: client,
		state: &State{
			IsRunning: false,
		},
		logCh: make(chan string, 1000), // Buffer for logs
	}
}

func (b *Bot) State() *State {
	return b.state
}

func (b *Bot) Start() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state.IsRunning {
		return
	}

	b.state.IsRunning = true
	b.stopCh = make(chan struct{})

	b.Log("Bot started")
	go b.loop()
}

func (b *Bot) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.state.IsRunning {
		return
	}
	b.state.IsRunning = false
	close(b.stopCh)
	b.Log("Bot stopped")
}

func (b *Bot) Log(msg string) {
	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("[%s] %s", ts, msg)
	select {
	case b.logCh <- line:
	default:
		<-b.logCh // drop oldest
		b.logCh <- line
	}
	log.Println(line)
}

func (b *Bot) GetNewLogs() []string {
	var logs []string
	for {
		select {
		case l := <-b.logCh:
			logs = append(logs, l)
		default:
			return logs
		}
	}
}
