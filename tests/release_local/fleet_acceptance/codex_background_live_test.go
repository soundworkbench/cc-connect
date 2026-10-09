package fleet_acceptance

import (
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real Codex + real engine; capture only the final network boundary. No existing
// conversation is reused and no business task is dispatched by this acceptance.
func TestFleetLiveBackgroundTwoBatches(t *testing.T) {
	if os.Getenv("FLEET_BACKGROUND_LIVE") != "1" || os.Getenv("FLEET_CONFIG") == "" {
		t.Skip("explicit live acceptance only")
	}
	cfg, err := config.Load(os.Getenv("FLEET_CONFIG"))
	if err != nil {
		t.Fatal("cannot load selected configuration")
	}
	project := cfg.Projects[0]
	if project.Agent.Type != "codex" {
		t.Skip("Codex acceptance")
	}
	opts := project.Agent.Options
	opts["cc_data_dir"] = t.TempDir()
	opts["work_dir"] = t.TempDir()
	opts["background_wakeup"] = true
	opts["background_wakeup_max_turns"] = 3
	opts["background_wakeup_timeout_mins"] = 5
	agent, err := core.CreateAgent(project.Agent.Type, opts)
	if err != nil {
		t.Fatal("agent initialization failed")
	}
	p := &capture{final: make(chan string, 32)}
	e := core.NewEngine("background-live", agent, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangChinese)
	defer e.Stop()
	e.SetDisplayConfig(core.DisplayCfg{Mode: "quiet", HideAgentFooter: true})
	e.SetReplyFooterEnabled(false)
	prompt := `Infrastructure acceptance only. Batch 1: spawn one child, ask it to sleep 5 seconds then reply ONE_DONE. Immediately finish your own turn with DISPATCH_ONE, without waiting. When a background completion event subsequently prompts you, collect and close that child, spawn exactly one second child to sleep 5 seconds then reply TWO_DONE, and immediately finish with DISPATCH_TWO without waiting. On the next completion event collect and close the second child, reply ALL_BATCHES_DONE, and finish. Exactly two batches. No file edits, network, MCP or business work.`
	e.ReceiveMessage(p, &core.Message{SessionKey: "capture:background", Platform: "capture", UserID: "health", MessageID: "initial", Content: prompt, ReplyCtx: "capture"})
	observed := map[string]bool{}
	reports := 0
	end := time.NewTimer(5 * time.Minute)
	defer end.Stop()
	for {
		select {
		case text := <-p.final:
			matched := false
			for _, m := range []string{"DISPATCH_ONE", "DISPATCH_TWO", "ALL_BATCHES_DONE"} {
				if strings.Contains(text, m) {
					observed[m] = true
					matched = true
				}
			}
			if matched {
				reports++
			}
			if observed["ALL_BATCHES_DONE"] {
				if reports < 3 || !observed["DISPATCH_ONE"] || !observed["DISPATCH_TWO"] {
					t.Fatal("missing platform-visible batch reports")
				}
				t.Log("BACKGROUND_FLEET_ACCEPTANCE real_engine=true real_codex=true automatic_batches=2 platform_reports=3")
				return
			}
		case <-end.C:
			t.Fatal("two-batch end-to-end acceptance timed out")
		}
	}
}
