// Package main 按全部覆盖率记录的语句数执行覆盖率门槛。
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// coverage 保存按代码语句数量加权的已覆盖与总语句数。
type coverage struct {
	Covered uint64
	Total   uint64
}

// profileRecord 保存同一文件和起止行列的块首次出现时的语句数，以及是否已有记录被执行。
type profileRecord struct {
	statements uint64
	covered    bool
}

// profileBlock 匹配覆盖记录，从右侧区分文件名、源码范围、语句数与执行次数。
var profileBlock = regexp.MustCompile(`^(.+):([0-9]+)\.([0-9]+),([0-9]+)\.([0-9]+)\s+([0-9]+)\s+([0-9]+)$`)

// main 将命令行参数交给可测试入口并设置退出码。
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 读取完整覆盖文件并检查门槛；不会按目录或文件排除记录。
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("covercheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	minimum := flags.Float64("min", 80, "最低语句覆盖百分比，范围 0–100")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法: covercheck [-min 80] coverage.out")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 || math.IsNaN(*minimum) || math.IsInf(*minimum, 0) || *minimum < 0 || *minimum > 100 {
		flags.Usage()
		return 2
	}
	file, err := os.Open(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return 1
	}
	defer file.Close()
	result, err := readCoverage(file)
	if err != nil {
		fmt.Fprintf(stderr, "covercheck: %v\n", err)
		return 1
	}
	percent := 100 * float64(result.Covered) / float64(result.Total)
	fmt.Fprintf(stdout, "语句覆盖率 %.4f%%（%d/%d），要求 >= %.4f%%\n", percent, result.Covered, result.Total, *minimum)
	if percent < *minimum {
		return 1
	}
	return 0
}

// readCoverage 严格解析 Go coverprofile；空输入、坏格式、语句总数溢出、零总语句，
// 以及同一文件和起止行列的重复块语句数不一致时均返回错误。
// 重复块（如 -coverpkg 让多个测试二进制输出同一块）按唯一块合并：语句数只计一次，
// 任一记录执行次数大于零即视为已覆盖，与 go tool cover 的合并结果一致。
func readCoverage(reader io.Reader) (coverage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return coverage{}, err
		}
		return coverage{}, errors.New("覆盖率文件为空")
	}
	mode := scanner.Text()
	if mode != "mode: set" && mode != "mode: count" && mode != "mode: atomic" {
		return coverage{}, errors.New("无效的覆盖率模式头")
	}
	var result coverage
	blocks := make(map[string]*profileRecord)
	line := 1
	for scanner.Scan() {
		line++
		matches := profileBlock.FindStringSubmatch(scanner.Text())
		if matches == nil || strings.TrimSpace(matches[1]) == "" {
			return coverage{}, fmt.Errorf("第 %d 行覆盖率记录格式错误", line)
		}
		values := make([]uint64, 6)
		for index, text := range matches[2:] {
			value, err := strconv.ParseUint(text, 10, 64)
			if err != nil {
				return coverage{}, fmt.Errorf("第 %d 行数字无效: %w", line, err)
			}
			values[index] = value
		}
		startLine, startColumn, endLine, endColumn := values[0], values[1], values[2], values[3]
		statements, count := values[4], values[5]
		if startLine == 0 || startColumn == 0 || endLine == 0 || endColumn == 0 || endLine < startLine || (endLine == startLine && endColumn < startColumn) {
			return coverage{}, fmt.Errorf("第 %d 行源码范围无效", line)
		}
		if mode == "mode: set" && count > 1 {
			return coverage{}, fmt.Errorf("第 %d 行 set 模式的计数应为 0 或 1", line)
		}
		key := fmt.Sprintf("%s:%d.%d,%d.%d", matches[1], startLine, startColumn, endLine, endColumn)
		block, seen := blocks[key]
		if !seen {
			if math.MaxUint64-result.Total < statements {
				return coverage{}, errors.New("语句总数溢出")
			}
			result.Total += statements
			block = &profileRecord{statements: statements}
			blocks[key] = block
		} else if block.statements != statements {
			return coverage{}, fmt.Errorf("第 %d 行语句数与之前记录不一致", line)
		}
		// 已覆盖语句只在块首次被标记为已覆盖时累加，因此不会超过唯一块总数。
		if count > 0 && !block.covered {
			block.covered = true
			result.Covered += statements
		}
	}
	if err := scanner.Err(); err != nil {
		return coverage{}, err
	}
	if result.Total == 0 {
		return coverage{}, errors.New("覆盖率文件没有可统计语句")
	}
	return result, nil
}
