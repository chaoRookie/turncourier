package main

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errorReader 为测试模拟底层覆盖率文件的读取错误。
type errorReader struct{}

// Read 在读取时返回固定错误，验证扫描器错误没有被忽略。
func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("模拟读取失败")
}

// TestReadCoverageWeighted 验证所有路径均计入语句权重，并支持三种标准模式。
func TestReadCoverageWeighted(t *testing.T) {
	for _, mode := range []string{"set", "count", "atomic"} {
		t.Run(mode, func(t *testing.T) {
			profile := "mode: " + mode + "\ncmd/main.go:1.1,3.2 8 1\ntools/check.go:2.1,2.8 2 0\nC:/space name/empty.go:1.1,1.2 0 0\n"
			got, err := readCoverage(strings.NewReader(profile))
			if err != nil || got.Covered != 8 || got.Total != 10 {
				t.Fatalf("覆盖率 = %#v, %v", got, err)
			}
		})
	}
	got, err := readCoverage(strings.NewReader("mode: count\na.go:1.1,2.2 4 100\n"))
	if err != nil || got != (coverage{4, 4}) {
		t.Fatalf("执行 100 次不应加重权重: %#v, %v", got, err)
	}
}

// TestReadCoverageMergesDuplicateBlocks 验证 -coverpkg 产生的同一文件和起止行列的重复块只计一次语句数，任一记录计数大于零即算已覆盖。
func TestReadCoverageMergesDuplicateBlocks(t *testing.T) {
	cases := []struct {
		profile string
		want    coverage
	}{
		{"mode: atomic\na.go:1.1,2.1 8 0\na.go:1.1,2.1 8 3\nb.go:1.1,2.1 2 0\n", coverage{8, 10}},
		{"mode: set\na.go:1.1,2.1 8 1\na.go:1.1,2.1 8 0\na.go:1.1,2.2 2 1\na.go:1.1,2.2 2 1\n", coverage{10, 10}},
		{"mode: count\na.go:1.1,2.1 18446744073709551615 0\na.go:1.1,2.1 18446744073709551615 1\n", coverage{math.MaxUint64, math.MaxUint64}},
	}
	for _, tc := range cases {
		if got, err := readCoverage(strings.NewReader(tc.profile)); err != nil || got != tc.want {
			t.Errorf("%q 覆盖率 = %#v, %v，期望 %#v", tc.profile, got, err, tc.want)
		}
	}
}

// TestReadCoverageRejectsMalformed 验证空文件、无效或缺失模式头、无效数字和范围、语句总数溢出及重复块语句数冲突均不能绕过门槛。
func TestReadCoverageRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"mode: invalid\n",
		"mode: set\n",
		"mode: set\na.go:1.1,1.2 0 0\n",
		"mode: set\n\n",
		"mode: set\nbad\n",
		"mode: set\n :1.1,1.2 1 1\n",
		"mode: set\na.go:1.1,1.2 NaN 0\n",
		"mode: count\na.go:1.1,1.2 2 -1\n",
		"mode: count\na.go:1.1,1.2 2 18446744073709551616\n",
		"mode: set\na.go:0.1,1.2 1 1\n",
		"mode: set\na.go:1.0,1.2 1 1\n",
		"mode: set\na.go:1.1,0.2 1 1\n",
		"mode: set\na.go:1.1,1.0 1 1\n",
		"mode: set\na.go:2.1,1.2 1 1\n",
		"mode: set\na.go:1.3,1.2 1 1\n",
		"mode: set\na.go:1.1,1.2 1 2\n",
	}
	for _, profile := range cases {
		if _, err := readCoverage(strings.NewReader(profile)); err == nil {
			t.Errorf("期望拒绝: %q", profile)
		}
	}
	// 以下样本含有效块且溢出回绕后总数不为零，只有对应校验生效才会以指定原因失败。
	reasons := []struct {
		profile string
		reason  string
	}{
		{"mode: invalid\na.go:1.1,1.2 1 1\n", "模式头"},
		{"a.go:1.1,2.1 1000 0\nb.go:1.1,2.1 8 1\n", "模式头"},
		{"mode: atomic\na.go:1.1,1.2 18446744073709551615 1\nb.go:1.1,1.2 3 0\n", "溢出"},
		{"mode: set\na.go:1.1,2.1 8 1\na.go:1.1,2.1 7 1\n", "语句数与之前记录不一致"},
	}
	for _, tc := range reasons {
		if _, err := readCoverage(strings.NewReader(tc.profile)); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%q 的错误 = %v，期望包含 %q", tc.profile, err, tc.reason)
		}
	}
}

// TestReadCoveragePropagatesReadErrors 验证头部及正文读取错误都会返回。
func TestReadCoveragePropagatesReadErrors(t *testing.T) {
	for _, reader := range []io.Reader{errorReader{}, io.MultiReader(strings.NewReader("mode: set\na.go:1.1,1.2 1 1\n"), errorReader{})} {
		if _, err := readCoverage(reader); err == nil || !strings.Contains(err.Error(), "模拟读取失败") {
			t.Fatalf("读取错误未保留: %v", err)
		}
	}
}

// writeProfile 保存不含真实项目内容的临时覆盖率样本。
func writeProfile(t *testing.T, profile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coverage.out")
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunThresholdBoundary 验证恰好 80% 通过、79.99% 失败且不是按块数量平均。
func TestRunThresholdBoundary(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		minimum string
		code    int
	}{
		{"exactly 80", "mode: set\na.go:1.1,2.1 8 1\na.go:3.1,4.1 2 0\n", "80", 0},
		{"below 80", "mode: atomic\na.go:1.1,2.1 7999 1\na.go:3.1,4.1 2001 0\n", "80", 1},
		{"decimal", "mode: count\na.go:1.1,2.1 7999 3\na.go:3.1,4.1 2001 0\n", "79.99", 0},
		{"zero", "mode: set\na.go:1.1,2.1 2 0\n", "0", 0},
		{"full", "mode: set\na.go:1.1,2.1 2 1\n", "100", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeProfile(t, tc.profile)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"-min", tc.minimum, path}, &stdout, &stderr); code != tc.code {
				t.Fatalf("退出码 = %d，期望 %d: %s%s", code, tc.code, &stdout, &stderr)
			}
			if !strings.Contains(stdout.String(), "语句覆盖率") || stderr.Len() != 0 {
				t.Fatalf("输出不符合预期: %s%s", &stdout, &stderr)
			}
		})
	}
}

// TestRunArgumentsAndFailures 验证帮助、非法阈值及缺失或损坏文件的退出码。
func TestRunArgumentsAndFailures(t *testing.T) {
	good := writeProfile(t, "mode: set\na.go:1.1,2.1 1 1\n")
	bad := writeProfile(t, "not coverage")
	cases := []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{good, good}, 2},
		{[]string{"-h"}, 0},
		{[]string{"-unknown"}, 2},
		{[]string{good + ".missing"}, 1},
		{[]string{bad}, 1},
		{[]string{good}, 0},
	}
	for _, invalid := range []string{"NaN", "+Inf", "-Inf", "-0.1", "100.1", "bad"} {
		cases = append(cases, struct {
			args []string
			code int
		}{[]string{"-min", invalid, good}, 2})
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != tc.code {
			t.Errorf("参数 %q 退出码 = %d，期望 %d: %s%s", tc.args, code, tc.code, &stdout, &stderr)
		}
	}
}
