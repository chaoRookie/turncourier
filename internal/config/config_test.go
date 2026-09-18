// Package config 的加载测试用临时目录中的真实文件覆盖默认值、严格校验与环境变量白名单。
package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// minimalConfig 只写必填字段，用于断言默认值与各类局部错误。
const minimalConfig = `
[mailbox]
address = "bot@example.invalid"

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid"]
`

// configWithMailboxKey 在 [mailbox] 表中额外写入一个键，用于未知键与凭据类键的用例。
func configWithMailboxKey(key string) string {
	return `
[mailbox]
address = "bot@example.invalid"
` + key + ` = "x"

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid"]
`
}

// writeConfig 在独立的临时目录写入指定权限的配置文件，并返回指向它的 Paths。
func writeConfig(t *testing.T, content string, mode os.FileMode) Paths {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "turncourier.toml")
	if err := os.WriteFile(file, []byte(content), mode); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}
	// os.WriteFile 会受 umask 影响，权限用例必须显式设置。
	if err := os.Chmod(file, mode); err != nil {
		t.Fatalf("设置配置文件权限失败: %v", err)
	}
	return Paths{ConfigFile: file, DataDir: dir, Database: filepath.Join(dir, "turncourier.db")}
}

// loadConfig 以默认权限写入配置并加载，返回配置、所用路径与错误。
func loadConfig(t *testing.T, content string) (Config, Paths, error) {
	t.Helper()
	paths := writeConfig(t, content, 0o600)
	cfg, err := Load(paths, envOf(nil))
	return cfg, paths, err
}

// requireErrorWithoutPath 断言返回了错误，且错误文本不含临时目录路径。
func requireErrorWithoutPath(t *testing.T, err error, paths Paths) {
	t.Helper()
	if err == nil {
		t.Fatal("Load 应当返回错误")
	}
	if strings.Contains(err.Error(), paths.DataDir) {
		t.Errorf("错误文本包含本机路径: %v", err)
	}
}

// TestLoadExampleConfig 确认仓库中的示例配置可以加载，且字段与示例内容一致。
func TestLoadExampleConfig(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "configs", "turncourier.example.toml"))
	if err != nil {
		t.Fatalf("读取示例配置失败: %v", err)
	}
	paths := writeConfig(t, string(content), 0o600)
	cfg, err := Load(paths, envOf(nil))
	if err != nil {
		t.Fatalf("Load 示例配置失败: %v", err)
	}
	want := Config{
		Mailbox:   Mailbox{Address: "bot@example.invalid", IMAPHost: "imap.qq.com", IMAPPort: 993, SMTPHost: "smtp.qq.com", SMTPPort: 465},
		Recipient: Recipient{Address: "me@example.invalid", AllowedSenders: []string{"me@example.invalid"}},
		Notify:    Notify{Events: []string{"waiting_input", "failed", "turn_completed"}},
		Security:  Security{TokenTTL: 168 * time.Hour},
		Paths:     paths,
	}
	if cfg.Mailbox != want.Mailbox || cfg.Recipient.Address != want.Recipient.Address || cfg.Security != want.Security || cfg.Paths != want.Paths {
		t.Errorf("Load = %+v; want %+v", cfg, want)
	}
	if !slices.Equal(cfg.Recipient.AllowedSenders, want.Recipient.AllowedSenders) {
		t.Errorf("AllowedSenders = %v; want %v", cfg.Recipient.AllowedSenders, want.Recipient.AllowedSenders)
	}
	if !slices.Equal(cfg.Notify.Events, want.Notify.Events) {
		t.Errorf("Notify.Events = %v; want %v", cfg.Notify.Events, want.Notify.Events)
	}
}

// TestLoadDefaults 只写必填字段时应得到清单规定的默认主机、端口、事件与令牌有效期。
func TestLoadDefaults(t *testing.T) {
	cfg, _, err := loadConfig(t, minimalConfig)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	wantMailbox := Mailbox{Address: "bot@example.invalid", IMAPHost: "imap.qq.com", IMAPPort: 993, SMTPHost: "smtp.qq.com", SMTPPort: 465}
	if cfg.Mailbox != wantMailbox {
		t.Errorf("Mailbox = %+v; want %+v", cfg.Mailbox, wantMailbox)
	}
	if cfg.Security.TokenTTL != 168*time.Hour {
		t.Errorf("TokenTTL = %s; want 168h", cfg.Security.TokenTTL)
	}
	wantEvents := []string{"waiting_input", "failed", "turn_completed"}
	if !slices.Equal(cfg.Notify.Events, wantEvents) {
		t.Errorf("Notify.Events = %v; want %v", cfg.Notify.Events, wantEvents)
	}
	// 返回的是默认事件的副本：调用方修改它之后再次加载，默认值不变。
	cfg.Notify.Events[0] = "mutated"
	cfg, _, err = loadConfig(t, minimalConfig)
	if err != nil {
		t.Fatalf("再次 Load 返回错误: %v", err)
	}
	if !slices.Equal(cfg.Notify.Events, wantEvents) {
		t.Errorf("修改返回值后默认事件被改变: %v; want %v", cfg.Notify.Events, wantEvents)
	}
}

// TestLoadNormalizesAddresses 验证地址与白名单都经过规范化后保存。
func TestLoadNormalizesAddresses(t *testing.T) {
	cfg, _, err := loadConfig(t, `
[mailbox]
address = "Bot@Example.Invalid"

[recipient]
address = "  Me@Example.Invalid  "
allowed_senders = ["Me@Example.Invalid", "Other@Example.Invalid"]
`)
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.Mailbox.Address != "bot@example.invalid" || cfg.Recipient.Address != "me@example.invalid" {
		t.Errorf("地址未规范化: %+v", cfg)
	}
	want := []string{"me@example.invalid", "other@example.invalid"}
	if !slices.Equal(cfg.Recipient.AllowedSenders, want) {
		t.Errorf("AllowedSenders = %v; want %v", cfg.Recipient.AllowedSenders, want)
	}
}

// TestLoadReportsAllMissingFields 验证三个必填项缺失时的错误同时出现在一个返回值中。
func TestLoadReportsAllMissingFields(t *testing.T) {
	_, paths, err := loadConfig(t, `
[mailbox]
imap_host = "imap.example.invalid"

[recipient]
`)
	requireErrorWithoutPath(t, err, paths)
	for _, want := range []string{"mailbox.address is required", "recipient.address is required", "allowed_senders"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误未提到 %s: %v", want, err)
		}
	}
}

// TestLoadRejectsUnknownKey 验证未知键被拒绝且错误文本包含键名。
func TestLoadRejectsUnknownKey(t *testing.T) {
	_, paths, err := loadConfig(t, configWithMailboxKey("passwrod"))
	requireErrorWithoutPath(t, err, paths)
	if !strings.Contains(err.Error(), "passwrod") {
		t.Errorf("错误未提到未知键: %v", err)
	}
	// 表数组的每个元素都会列出同一键路径，同一个未知键只报一次。
	_, paths, err = loadConfig(t, "[[extra]]\nkey = 1\n[[extra]]\nkey = 2\n"+minimalConfig)
	requireErrorWithoutPath(t, err, paths)
	if count := strings.Count(err.Error(), "unknown key extra.key"); count != 1 {
		t.Errorf("unknown key extra.key 出现 %d 次; want 1: %v", count, err)
	}
}

// TestLoadRejectsCaseVariantKeys 验证键名逐段区分大小写：TOML 库会把大小写变体匹配到字段，
// 这些键必须按未知键拒绝，而不是被静默采用。
func TestLoadRejectsCaseVariantKeys(t *testing.T) {
	cases := []struct {
		old, new string
		keys     []string
	}{
		{`address = "bot@`, `ADDRESS = "bot@`, []string{"mailbox.ADDRESS"}},
		{"allowed_senders", "Allowed_Senders", []string{"recipient.Allowed_Senders"}},
		{"[mailbox]", "[MAILBOX]", []string{"MAILBOX", "MAILBOX.address"}},
	}
	for _, testCase := range cases {
		_, paths, err := loadConfig(t, strings.Replace(minimalConfig, testCase.old, testCase.new, 1))
		requireErrorWithoutPath(t, err, paths)
		for _, key := range testCase.keys {
			if !strings.Contains(err.Error(), "unknown key "+key) {
				t.Errorf("%s: 错误未把 %s 报为未知键: %v", testCase.new, key, err)
			}
		}
	}
}

// TestLoadRejectsDuplicateCaseVariants 验证同一字段写成两个大小写变体时必须报错，且多次加载的结果完全一致；
// 采用哪个取值由 map 遍历顺序决定，若继续校验取值，错误文本会随遍历顺序变化。
func TestLoadRejectsDuplicateCaseVariants(t *testing.T) {
	paths := writeConfig(t, configWithMailboxKey("ADDRESS"), 0o600)
	var first string
	for i := range 20 {
		_, err := Load(paths, envOf(nil))
		requireErrorWithoutPath(t, err, paths)
		if i == 0 {
			first = err.Error()
			if !strings.Contains(first, "unknown key mailbox.ADDRESS") {
				t.Fatalf("错误未把 mailbox.ADDRESS 报为未知键: %v", err)
			}
			continue
		}
		if err.Error() != first {
			t.Fatalf("第 %d 次加载的错误与第 1 次不同:\n%v\n---\n%s", i+1, err, first)
		}
	}
}

// TestLoadRejectsCredentialKeys 验证凭据类键被拒绝，并提示凭据由 init 写入 Keychain；
// 键名按小写比较，大写或混合大小写的凭据类键同样给出 Keychain 提示。
func TestLoadRejectsCredentialKeys(t *testing.T) {
	for _, key := range []string{"password", "authorization_code", "auth_code", "token", "secret", "PASSWORD", "Secret"} {
		_, paths, err := loadConfig(t, configWithMailboxKey(key))
		requireErrorWithoutPath(t, err, paths)
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: 错误未提到键名: %v", key, err)
		}
		if !strings.Contains(err.Error(), "凭据将由 init 写入 Keychain") {
			t.Errorf("%s: 错误缺少 Keychain 提示: %v", key, err)
		}
	}
}

// TestLoadRejectsInvalidAddresses 验证三处地址字段非法时都包装 ErrInvalidAddress。
func TestLoadRejectsInvalidAddresses(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"机器人地址", `
[mailbox]
address = "Bot <bot@example.invalid>"

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid"]
`},
		{"收件人地址", `
[mailbox]
address = "bot@example.invalid"

[recipient]
address = "me@example"
allowed_senders = ["me@example.invalid"]
`},
		{"白名单地址", `
[mailbox]
address = "bot@example.invalid"

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid", "用户@example.invalid"]
`},
	}
	for _, testCase := range cases {
		_, paths, err := loadConfig(t, testCase.content)
		requireErrorWithoutPath(t, err, paths)
		if !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("%s: 错误 = %v; want ErrInvalidAddress", testCase.name, err)
		}
	}
}

// TestLoadRejectsSenderListProblems 验证白名单重复与包含机器人自身地址都会报错。
func TestLoadRejectsSenderListProblems(t *testing.T) {
	cases := []struct {
		name    string
		senders string
	}{
		{"规范化后重复", `["me@example.invalid", "Me@Example.Invalid"]`},
		{"包含机器人地址", `["me@example.invalid", "bot@example.invalid"]`},
	}
	for _, testCase := range cases {
		_, paths, err := loadConfig(t, `
[mailbox]
address = "bot@example.invalid"

[recipient]
address = "me@example.invalid"
allowed_senders = `+testCase.senders+"\n")
		requireErrorWithoutPath(t, err, paths)
	}
}

// TestLoadEvents 验证事件列表：空列表合法，未知事件与重复事件报错。
func TestLoadEvents(t *testing.T) {
	cfg, _, err := loadConfig(t, minimalConfig+`
[notify]
events = []
`)
	if err != nil {
		t.Fatalf("空事件列表应当合法: %v", err)
	}
	if len(cfg.Notify.Events) != 0 {
		t.Errorf("Notify.Events = %v; want 空列表", cfg.Notify.Events)
	}
	for _, events := range []string{`["nope"]`, `["failed", "failed"]`} {
		_, paths, err := loadConfig(t, minimalConfig+`
[notify]
events = `+events+"\n")
		requireErrorWithoutPath(t, err, paths)
	}
}

// TestLoadPorts 验证端口边界：0 与 65536 报错，1 与 65535 合法。
func TestLoadPorts(t *testing.T) {
	for _, port := range []string{"0", "65536"} {
		_, paths, err := loadConfig(t, `
[mailbox]
address = "bot@example.invalid"
imap_port = `+port+`

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid"]
`)
		requireErrorWithoutPath(t, err, paths)
	}
	cfg, _, err := loadConfig(t, `
[mailbox]
address = "bot@example.invalid"
imap_port = 1
smtp_port = 65535

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid"]
`)
	if err != nil {
		t.Fatalf("边界端口应当合法: %v", err)
	}
	if cfg.Mailbox.IMAPPort != 1 || cfg.Mailbox.SMTPPort != 65535 {
		t.Errorf("端口 = %d、%d; want 1、65535", cfg.Mailbox.IMAPPort, cfg.Mailbox.SMTPPort)
	}
}

// TestLoadHosts 验证主机名含协议前缀、空白或为空时报错，合法且非默认的主机名被采用。
func TestLoadHosts(t *testing.T) {
	for _, host := range []string{`"imaps://imap.example.invalid"`, `"imap example invalid"`, `""`} {
		_, paths, err := loadConfig(t, `
[mailbox]
address = "bot@example.invalid"
imap_host = `+host+`

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid"]
`)
		requireErrorWithoutPath(t, err, paths)
	}
	cfg, _, err := loadConfig(t, `
[mailbox]
address = "bot@example.invalid"
imap_host = "imap.163.com"
smtp_host = "smtp.163.com"

[recipient]
address = "me@example.invalid"
allowed_senders = ["me@example.invalid"]
`)
	if err != nil {
		t.Fatalf("合法主机名应当通过: %v", err)
	}
	if cfg.Mailbox.IMAPHost != "imap.163.com" || cfg.Mailbox.SMTPHost != "smtp.163.com" {
		t.Errorf("主机名 = %s、%s; want imap.163.com、smtp.163.com", cfg.Mailbox.IMAPHost, cfg.Mailbox.SMTPHost)
	}
}

// TestLoadTokenTTL 验证令牌有效期的解析与 1h 到 720h 的范围限制。
func TestLoadTokenTTL(t *testing.T) {
	for _, ttl := range []string{"59m", "721h", "abc"} {
		_, paths, err := loadConfig(t, minimalConfig+`
[security]
token_ttl = "`+ttl+`"
`)
		requireErrorWithoutPath(t, err, paths)
	}
	for _, ttl := range []struct {
		text string
		want time.Duration
	}{{"1h", time.Hour}, {"720h", 720 * time.Hour}} {
		cfg, _, err := loadConfig(t, minimalConfig+`
[security]
token_ttl = "`+ttl.text+`"
`)
		if err != nil {
			t.Fatalf("token_ttl=%s 应当合法: %v", ttl.text, err)
		}
		if cfg.Security.TokenTTL != ttl.want {
			t.Errorf("TokenTTL = %s; want %s", cfg.Security.TokenTTL, ttl.want)
		}
	}
}

// TestLoadNotifyEventsEnv 验证唯一的环境变量覆盖：合法值整体替换文件配置，空串视为未设置，非法值报错。
func TestLoadNotifyEventsEnv(t *testing.T) {
	content := minimalConfig + `
[notify]
events = ["waiting_approval"]
`
	paths := writeConfig(t, content, 0o600)
	cfg, err := Load(paths, envOf(map[string]string{"TURNCOURIER_NOTIFY_EVENTS": "failed, waiting_input"}))
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	want := []string{"failed", "waiting_input"}
	if !slices.Equal(cfg.Notify.Events, want) {
		t.Errorf("Notify.Events = %v; want %v", cfg.Notify.Events, want)
	}

	cfg, err = Load(paths, envOf(map[string]string{"TURNCOURIER_NOTIFY_EVENTS": ""}))
	if err != nil {
		t.Fatalf("空环境变量应视为未设置: %v", err)
	}
	if !slices.Equal(cfg.Notify.Events, []string{"waiting_approval"}) {
		t.Errorf("Notify.Events = %v; want [waiting_approval]", cfg.Notify.Events)
	}

	_, err = Load(paths, envOf(map[string]string{"TURNCOURIER_NOTIFY_EVENTS": "failed,nope"}))
	requireErrorWithoutPath(t, err, paths)
}

// TestLoadReadsOnlyNotifyEventsEnv 验证 Load 只读取 TURNCOURIER_NOTIFY_EVENTS：
// 其他看似可覆盖安全相关项的环境变量即使给出攻击值也不会被读取，结果与文件内容一致。
func TestLoadReadsOnlyNotifyEventsEnv(t *testing.T) {
	paths := writeConfig(t, minimalConfig, 0o600)
	want, err := Load(paths, envOf(nil))
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	attack := map[string]string{
		"TURNCOURIER_ALLOWED_SENDERS":   "attacker@example.invalid",
		"TURNCOURIER_TOKEN_TTL":         "720h",
		"TURNCOURIER_MAILBOX_ADDRESS":   "attacker@example.invalid",
		"TURNCOURIER_RECIPIENT_ADDRESS": "attacker@example.invalid",
		"TURNCOURIER_IMAP_HOST":         "imap.attacker.invalid",
		"TURNCOURIER_SMTP_HOST":         "smtp.attacker.invalid",
	}
	var read []string
	got, err := Load(paths, func(key string) string {
		read = append(read, key)
		return attack[key]
	})
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if !slices.Equal(read, []string{"TURNCOURIER_NOTIFY_EVENTS"}) {
		t.Errorf("读取的环境变量 = %v; want 只有 TURNCOURIER_NOTIFY_EVENTS", read)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %+v; want %+v", got, want)
	}
}

// TestLoadFileProblems 验证文件缺失、过大与不是常规文件时的错误。
func TestLoadFileProblems(t *testing.T) {
	dir := t.TempDir()
	missing := Paths{ConfigFile: filepath.Join(dir, "turncourier.toml"), DataDir: dir}
	_, err := Load(missing, envOf(nil))
	requireErrorWithoutPath(t, err, missing)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("错误 = %v; want ErrNotFound", err)
	}

	_, paths, err := loadConfig(t, minimalConfig+"\n# "+strings.Repeat("a", 1<<20)+"\n")
	requireErrorWithoutPath(t, err, paths)

	directory := t.TempDir()
	target := filepath.Join(directory, "turncourier.toml")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	asDirectory := Paths{ConfigFile: target, DataDir: directory}
	_, err = Load(asDirectory, envOf(nil))
	requireErrorWithoutPath(t, err, asDirectory)
	// 目录也能被 os.Open 打开，只有断言拒绝原因才能钉住常规文件检查。
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("错误 = %v; want 常规文件检查失败", err)
	}

	// 父级是普通文件时打开失败，文件系统错误须剥离路径，且不能当作文件不存在。
	parentDir := t.TempDir()
	parent := filepath.Join(parentDir, "not-a-directory")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatalf("写入普通文件失败: %v", err)
	}
	underFile := Paths{ConfigFile: filepath.Join(parent, "turncourier.toml"), DataDir: parentDir}
	_, err = Load(underFile, envOf(nil))
	requireErrorWithoutPath(t, err, underFile)
	if errors.Is(err, ErrNotFound) {
		t.Errorf("错误 = %v; 父级是普通文件不应视为文件不存在", err)
	}
}

// TestReadConfigDataRejectsOversize 验证读取阶段自身也限制大小：文件在检查大小之后被写大时，
// 读到超过上界的内容即拒绝，而不是把截断后的内容交给解析器。
func TestReadConfigDataRejectsOversize(t *testing.T) {
	paths := writeConfig(t, minimalConfig+"\n# "+strings.Repeat("a", maxConfigBytes)+"\n", 0o600)
	file, err := os.Open(paths.ConfigFile)
	if err != nil {
		t.Fatalf("打开配置文件失败: %v", err)
	}
	defer file.Close()
	data, err := readConfigData(file)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("readConfigData 读到 %d 字节, 错误 = %v; want 超过上界的错误", len(data), err)
	}

	exact := writeConfig(t, strings.Repeat("#", maxConfigBytes), 0o600)
	file, err = os.Open(exact.ConfigFile)
	if err != nil {
		t.Fatalf("打开配置文件失败: %v", err)
	}
	defer file.Close()
	if data, err := readConfigData(file); err != nil || len(data) != maxConfigBytes {
		t.Errorf("恰好等于上界: 读到 %d 字节, 错误 = %v; want %d 字节且无错误", len(data), err, maxConfigBytes)
	}
}

// TestLoadRejectsBrokenTOML 验证语法错误的配置被拒绝，且错误文本不含本机路径。
func TestLoadRejectsBrokenTOML(t *testing.T) {
	_, paths, err := loadConfig(t, "[mailbox\naddress = \"bot@example.invalid\"\n")
	requireErrorWithoutPath(t, err, paths)
}

// TestLoadParseErrorsHideValues 验证 TOML 语法与类型错误不回显出错位置的原始文本：
// 未加引号的凭据值或普通值都不会进入错误文本，错误仍给出行号。
func TestLoadParseErrorsHideValues(t *testing.T) {
	cases := []struct {
		name, line, value, want string
	}{
		{"未加引号的凭据值", "authorization_code = abcdefghijklmnop", "abcdefghijklmnop", "凭据将由 init 写入 Keychain"},
		{"未加引号的普通值", "imap_host = imapsecretvalue", "imapsecretvalue", "invalid TOML"},
		{"类型错误", `imap_port = "portsecretvalue"`, "portsecretvalue", "mailbox.imap_port"},
	}
	for _, testCase := range cases {
		// 出错的键写在第 3 行。
		_, paths, err := loadConfig(t, "[mailbox]\naddress = \"bot@example.invalid\"\n"+testCase.line+"\n")
		requireErrorWithoutPath(t, err, paths)
		if strings.Contains(err.Error(), testCase.value) {
			t.Errorf("%s: 错误文本回显了原始取值: %v", testCase.name, err)
		}
		for _, want := range []string{"line 3", testCase.want} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: 错误未包含 %q: %v", testCase.name, want, err)
			}
		}
	}
}

// TestLoadFilePermissions 仅在 Unix 上验证组或其他用户可写的配置文件被拒绝。
func TestLoadFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("文件属主与权限检查只在 Unix 生效")
	}
	// 0o620 只让组可写，用来钉住权限掩码同时覆盖组与其他用户。
	for _, mode := range []os.FileMode{0o622, 0o602, 0o620} {
		paths := writeConfig(t, minimalConfig, mode)
		_, err := Load(paths, envOf(nil))
		requireErrorWithoutPath(t, err, paths)
	}
	for _, mode := range []os.FileMode{0o600, 0o644} {
		paths := writeConfig(t, minimalConfig, mode)
		if _, err := Load(paths, envOf(nil)); err != nil {
			t.Errorf("权限 %o 应当通过: %v", mode, err)
		}
	}
}
