package feishu_test

// Register the live-test agent in the external test assembly, not in the
// Feishu package: production platform code must never depend on an agent.
import _ "github.com/chenhg5/cc-connect/agent/claudecode"
