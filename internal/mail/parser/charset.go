// Package parser 的字符集处理：在包初始化时把 GBK 系列的标签映射到 GB18030 解码器，并判断一个文本部件的正文是否经过了
// 字符集转换（只有经过转换的正文，解码器写出的替换字符才说明解码失败）。
package parser

import (
	"strings"

	"github.com/emersion/go-message/charset"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// gbkLabels 是映射到 GB18030 解码器的字符集标签。QQ、Foxmail 等客户端标注 gbk 或 gb2312 的邮件里可能含 GB18030 的四字节字符
// （扩展区汉字、表情），GBK 解码器会把它们换成替换字符；GB18030 是这些编码的超集，用它解码不会损坏原本合法的文字。
var gbkLabels = []string{"gbk", "gb2312", "cp936", "x-gbk", "windows-936"}

// init 注册 gbkLabels 的映射。go-message/charset 的标签表是进程级的：导入本包的进程中，go-message 对这些标签的解码
// （正文部件与头部的编码词）都改用 GB18030。注册只在包初始化时进行，此后只读，不存在并发写入；go-message 查表前会把标签
// 转成小写，所以只注册小写形式。
func init() {
	for _, label := range gbkLabels {
		charset.RegisterEncoding(label, simplifiedchinese.GB18030)
	}
}

// converted 判断 charset 参数为 label 的文本部件是否经过 go-message 的字符集转换：utf-8、us-ascii 与缺省（空）按原样传递字节，
// 其余标签都交给解码器。x/text 的解码器遇到非法字节不报错，而是写出替换字符 U+FFFD。
func converted(label string) bool {
	switch strings.ToLower(label) {
	case "", "utf-8", "us-ascii":
		return false
	}
	return true
}
