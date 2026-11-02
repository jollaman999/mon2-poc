package vsphere

import (
	"strings"

	"github.com/gobwas/glob"
)

// Telegraf filter 패키지(Compile · NewIncludeExcludeFilter)를 그대로 옮겼다. 문법은 gobwas/glob 이다.
// 패턴에 * ? [ 가 하나도 없으면 글롭을 컴파일하지 않고 문자열이 같은지만 본다.

type matcher interface {
	Match(string) bool
}

// compileFilter 는 패턴 목록을 matcher 로 만든다. 목록이 비면 nil 이다.
// separators 를 주면 * 가 그 문자를 건너지 않는다.
func compileFilter(filters []string, separators ...rune) (matcher, error) {
	if len(filters) == 0 {
		return nil, nil
	}

	wildcards := len(separators) != 0
	for _, filter := range filters {
		if strings.ContainsAny(filter, "*?[") {
			wildcards = true
			break
		}
	}

	switch {
	case !wildcards && len(filters) == 1:
		return &filterSingle{s: filters[0]}, nil
	case !wildcards:
		return newFilterNoGlob(filters), nil
	case len(filters) == 1:
		return glob.Compile(filters[0], separators...)
	default:
		return newFilterGlobMultiple(filters, separators...)
	}
}

type filterSingle struct {
	s string
}

func (f *filterSingle) Match(s string) bool {
	return f.s == s
}

type filterNoGlob struct {
	m map[string]struct{}
}

func newFilterNoGlob(filters []string) matcher {
	out := filterNoGlob{m: make(map[string]struct{})}
	for _, filter := range filters {
		out.m[filter] = struct{}{}
	}
	return &out
}

func (f *filterNoGlob) Match(s string) bool {
	_, ok := f.m[s]
	return ok
}

type filterGlobMultiple struct {
	set []glob.Glob
}

func newFilterGlobMultiple(filters []string, separators ...rune) (matcher, error) {
	f := &filterGlobMultiple{set: make([]glob.Glob, 0, len(filters))}
	for _, pattern := range filters {
		g, err := glob.Compile(pattern, separators...)
		if err != nil {
			return nil, err
		}
		f.set = append(f.set, g)
	}
	return f, nil
}

func (f *filterGlobMultiple) Match(s string) bool {
	for _, g := range f.set {
		if g.Match(s) {
			return true
		}
	}
	return false
}

// includeExclude 는 include 가 비면 모두 들이고, exclude 에 맞으면 뺀다.
type includeExclude struct {
	include matcher
	exclude matcher
}

func newIncludeExclude(include, exclude []string) (includeExclude, error) {
	in, err := compileFilter(include)
	if err != nil {
		return includeExclude{}, err
	}
	ex, err := compileFilter(exclude)
	if err != nil {
		return includeExclude{}, err
	}
	return includeExclude{include: in, exclude: ex}, nil
}

func (f includeExclude) Match(s string) bool {
	if f.include != nil && !f.include.Match(s) {
		return false
	}
	return f.exclude == nil || !f.exclude.Match(s)
}
