package unifiedlog

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// resolveFiles 는 파일 패턴을 읽을 경로 목록으로 푼다. 중복은 inode(dev+ino)로 거른다.
// Rocky/RHEL 처럼 /var/log/syslog -> /var/log/messages 심볼릭 링크가 있으면 한 번만 읽는다.
// stat 이 실패하거나 inode 를 못 구하는 OS 면 경로 문자열로만 거른다. 먼저 나온 경로가 남는다.
func resolveFiles(patterns []string) []string {
	var out []string
	seenPath := make(map[string]struct{})
	seenInode := make(map[string]struct{})

	consider := func(path string) {
		if _, ok := seenPath[path]; ok {
			return
		}
		seenPath[path] = struct{}{}
		if key := inodeKey(path); key != "" {
			if _, ok := seenInode[key]; ok {
				return
			}
			seenInode[key] = struct{}{}
		}
		out = append(out, path)
	}

	for _, pattern := range patterns {
		for _, m := range globMatch(pattern) {
			consider(m)
		}
	}
	return out
}

// globMatch 는 *, ?, [] 와 ** (여러 단계 디렉터리)를 지원한다. 일반 파일만 돌려준다.
func globMatch(pattern string) []string {
	if !strings.ContainsAny(pattern, "*?[") {
		if fi, err := os.Stat(pattern); err == nil && fi.Mode().IsRegular() {
			return []string{pattern}
		}
		return nil
	}
	var out []string
	if !strings.Contains(pattern, "**") {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil
		}
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
				out = append(out, m)
			}
		}
		return out
	}
	re, err := regexp.Compile(globToRegexp(filepath.ToSlash(pattern)))
	if err != nil {
		return nil
	}
	root := staticPrefix(pattern)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if re.MatchString(filepath.ToSlash(p)) {
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				out = append(out, p)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// staticPrefix 는 패턴에서 글롭 문자가 처음 나오기 전의 디렉터리다.
func staticPrefix(pattern string) string {
	i := strings.IndexAny(pattern, "*?[")
	dir := filepath.Dir(pattern[:i] + "x")
	if dir == "" {
		return "."
	}
	return dir
}

func globToRegexp(p string) string {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				// "**/" 는 0 단계 이상의 디렉터리
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			cls := p[i+1 : i+j]
			if strings.HasPrefix(cls, "!") {
				cls = "^" + cls[1:]
			}
			b.WriteString("[" + cls + "]")
			i += j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteByte('$')
	return b.String()
}
