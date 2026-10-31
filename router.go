package main

import (
	"fmt"
	"strings"
)

// compileRules は決まりを当てはめる。first の決まり (断るもの) を先に、次に完全一致 (map で 1 回)、残りは書いた順に見て、最初に合ったもの。
// 同じパスの完全一致が 2 つあってはいけない
func compileRules(rules []Rule) func(path string, c *Ctx) Handler {
	exact := map[string]Handler{}
	var first, rest []Rule
	for _, r := range rules {
		switch {
		case r.hasExact:
			if _, dup := exact[r.exact]; dup {
				panic(fmt.Sprintf("duplicate exact rule: %s", r.exact))
			}
			exact[r.exact] = r.handler
		case r.first:
			first = append(first, r)
		default:
			rest = append(rest, r)
		}
	}
	return func(path string, c *Ctx) Handler {
		for i := range first {
			r := &first[i]
			if r.fn != nil {
				if _, ok := r.fn(path); ok {
					return r.handler
				}
			} else if strings.HasPrefix(path, r.prefix) {
				return r.handler
			}
		}
		if h, ok := exact[path]; ok {
			return h
		}
		for i := range rest {
			r := &rest[i]
			if r.fn != nil {
				if cap, ok := r.fn(path); ok {
					c.cap = cap
					return r.handler
				}
			} else if strings.HasPrefix(path, r.prefix) {
				return r.handler
			}
		}
		return nil
	}
}

func exactRule(path string, h Handler) Rule    { return Rule{exact: path, hasExact: true, handler: h} }
func prefixRule(prefix string, h Handler) Rule { return Rule{prefix: prefix, handler: h} }
func fnRule(fn func(string) (string, bool), h Handler) Rule {
	return Rule{fn: fn, handler: h}
}
