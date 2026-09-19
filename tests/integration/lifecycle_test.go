// Package integration_test 组合示例配置、临时目录中的真实 SQLite 存储与任务、回复队列状态机，
// 验证回复从入队、派发、崩溃后恢复到任务关闭的完整生命周期；只用合成数据，不访问网络。
package integration_test

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/chaoRookie/turncourier/internal/config"
	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/store/sqlite"
	"github.com/chaoRookie/turncourier/internal/task"
)

// openStore 打开 dataDir 中的存储并在测试结束时关闭；已关闭的存储再次关闭的错误被忽略。
func openStore(t *testing.T, dataDir string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.Context(), dataDir, sqlite.Options{})
	if err != nil {
		t.Fatalf("sqlite.Open 返回错误: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// syntheticReply 为任务构造一封合成入站回复的元数据，摘要取自合成正文。
func syntheticReply(taskID, account string, uid uint32, messageID, body string) sqlite.InboundReply {
	return sqlite.InboundReply{
		TaskID:      taskID,
		Account:     account,
		UIDValidity: 1,
		UID:         uid,
		MessageID:   messageID,
		BodySHA256:  sha256.Sum256([]byte(body)),
	}
}

// expectReply 断言回复操作成功，且回复序号、回复状态与任务状态符合预期。
func expectReply(t *testing.T, step string, reply sqlite.Reply, current sqlite.Task, err error, seq int64, state queue.State, taskState task.State) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s 返回错误: %v", step, err)
	}
	if reply.Seq != seq || reply.State != state || current.State != taskState {
		t.Fatalf("%s = 回复 %d/%s、任务 %s; want 回复 %d/%s、任务 %s",
			step, reply.Seq, reply.State, current.State, seq, state, taskState)
	}
}

// applyEvent 执行任务事件，断言成功且任务进入 want 状态。
func applyEvent(t *testing.T, store *sqlite.Store, current sqlite.Task, event task.Event, want task.State) {
	t.Helper()
	next, err := store.ApplyTaskEvent(t.Context(), current.ID, current.Version, event)
	if err != nil {
		t.Fatalf("%s 返回错误: %v", event, err)
	}
	if next.State != want {
		t.Fatalf("%s 后任务为 %s; want %s", event, next.State, want)
	}
}

// TestReplyLifecycle 走完一个任务的完整生命周期：加载示例配置，创建并启动任务，两条回复先后入队；
// 第 1 条派发后投递不确定，模拟进程崩溃后重开存储恢复并核对为已送达；第 2 条派发确认后关闭任务，
// 最后验证重复邮件去重、关闭后新邮件被拒绝，以及完整的任务事件序列。
func TestReplyLifecycle(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "turncourier.example.toml"))
	if err != nil {
		t.Fatalf("读取示例配置失败: %v", err)
	}
	configFile := filepath.Join(dir, "turncourier.toml")
	if err := os.WriteFile(configFile, example, 0o600); err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}
	// t.TempDir 通常是 0755，会被数据目录的权限检查拒绝；使用尚不存在的子目录，由 sqlite.Open 以 0700 创建。
	dataDir := filepath.Join(dir, "data")
	t.Setenv("TURNCOURIER_CONFIG", configFile)
	t.Setenv("TURNCOURIER_DATA_DIR", dataDir)
	// 清空通知事件覆盖，避免开发者环境中的取值影响加载结果。
	t.Setenv("TURNCOURIER_NOTIFY_EVENTS", "")

	paths, err := config.ResolvePaths(os.Getenv, os.UserConfigDir)
	if err != nil {
		t.Fatalf("ResolvePaths 返回错误: %v", err)
	}
	cfg, err := config.Load(paths, os.Getenv)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.Paths.DataDir != dataDir {
		t.Fatalf("DataDir = %q; want %q", cfg.Paths.DataDir, dataDir)
	}

	store := openStore(t, cfg.Paths.DataDir)
	// 配置与存储各自定义数据库文件名；存储打开后 cfg.Paths.Database 须恰好是它创建的数据库文件。
	info, err := os.Stat(cfg.Paths.Database)
	if err != nil {
		t.Fatalf("存储打开后 cfg.Paths.Database 不存在: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("cfg.Paths.Database 的类型 = %v; want 常规文件", info.Mode().Type())
	}
	created, err := store.CreateTask(ctx, sqlite.AgentCodex)
	if err != nil {
		t.Fatalf("CreateTask 返回错误: %v", err)
	}
	id := created.ID
	current, err := store.StartTask(ctx, id, created.Version, "thread-synthetic-0001")
	if err != nil {
		t.Fatalf("StartTask 返回错误: %v", err)
	}
	applyEvent(t, store, current, task.TurnCompleted, task.Completed)

	first := syntheticReply(id, cfg.Mailbox.Address, 1, "<first@example.invalid>", "first synthetic reply")
	second := syntheticReply(id, cfg.Mailbox.Address, 2, "<second@example.invalid>", "second synthetic reply")
	var seqs []int64
	for _, in := range []sqlite.InboundReply{first, second} {
		result, err := store.RecordReply(ctx, in)
		if err != nil {
			t.Fatalf("RecordReply(%s) 返回错误: %v", in.MessageID, err)
		}
		if result.Duplicate || result.Reply.State != queue.Queued {
			t.Fatalf("RecordReply(%s) = %+v; want 新入队 QUEUED", in.MessageID, result)
		}
		seqs = append(seqs, result.Reply.Seq)
	}

	reply, current, err := store.ClaimNextReply(ctx, id)
	expectReply(t, "派发第 1 条", reply, current, err, seqs[0], queue.Dispatching, task.Running)
	reply, current, err = store.MarkReplyUncertain(ctx, reply.Seq)
	expectReply(t, "MarkReplyUncertain", reply, current, err, seqs[0], queue.Uncertain, task.DeliveryUncertain)

	// 关闭存储模拟进程崩溃，重新打开后只能经本地核对处理在途回复，不会自动重发。
	if err := store.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	store = openStore(t, cfg.Paths.DataDir)
	recovered, err := store.RecoverInFlight(ctx)
	if err != nil {
		t.Fatalf("RecoverInFlight 返回错误: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Seq != seqs[0] || recovered[0].State != queue.Uncertain {
		t.Fatalf("RecoverInFlight = %+v; want 仅第 1 条 UNCERTAIN 回复", recovered)
	}
	reply, current, err = store.ResolveUncertainReply(ctx, seqs[0], true)
	expectReply(t, "ResolveUncertainReply(true)", reply, current, err, seqs[0], queue.Acknowledged, task.Running)
	applyEvent(t, store, current, task.TurnCompleted, task.Completed)

	reply, current, err = store.ClaimNextReply(ctx, id)
	expectReply(t, "派发第 2 条", reply, current, err, seqs[1], queue.Dispatching, task.Running)
	reply, current, err = store.AcknowledgeReply(ctx, reply.Seq)
	expectReply(t, "AcknowledgeReply", reply, current, err, seqs[1], queue.Acknowledged, task.Running)
	applyEvent(t, store, current, task.Close, task.Closed)

	again, err := store.RecordReply(ctx, first)
	if err != nil {
		t.Fatalf("再次记录第 1 条返回错误: %v", err)
	}
	if !again.Duplicate || again.Reply.Seq != seqs[0] {
		t.Fatalf("再次记录第 1 条 = %+v; want Duplicate=true、seq %d", again, seqs[0])
	}
	third, err := store.RecordReply(ctx, syntheticReply(id, cfg.Mailbox.Address, 3, "<third@example.invalid>", "third synthetic reply"))
	if err != nil {
		t.Fatalf("记录第 3 条返回错误: %v", err)
	}
	if third.Duplicate || third.Reply.State != queue.Rejected || third.Reply.RejectReason != "task_closed" {
		t.Fatalf("记录第 3 条 = %+v; want REJECTED(task_closed)", third)
	}

	events, err := store.TaskEvents(ctx, id)
	if err != nil {
		t.Fatalf("TaskEvents 返回错误: %v", err)
	}
	var got []task.Event
	for _, event := range events {
		got = append(got, event.Event)
	}
	want := []task.Event{
		task.Start, task.TurnCompleted, task.ReplyDispatched, task.DeliveryUnknown,
		task.DeliveryConfirmed, task.TurnCompleted, task.ReplyDispatched, task.Close,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("任务事件 = %v; want %v", got, want)
	}
}
