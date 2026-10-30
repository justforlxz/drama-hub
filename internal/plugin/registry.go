package plugin

import (
	"errors"
	"fmt"
	"sync"
)

// Registry 是插件的注册表：名字 → 插件，注册顺序稳定。
//
// 显式注册，不用 init()（ADR-0006 第 5 条）：装配根（cmd/drama-hub）持有唯一实例并
// 逐个 Register，包里没有任何自动注册入口。Registry 可并发读（框架会并行取多部剧），
// Register 只在启动装配阶段调用。
type Registry struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]Plugin
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Plugin)}
}

// Register 注册一个插件。以下情况返回错误而不 panic：p == nil、p.Name() == ""、
// 与前一个插件重名。错误文案只用于排障，调用方不需要按哨兵判定。
func (r *Registry) Register(p Plugin) error {
	if p == nil {
		return errors.New("plugin: 不能注册 nil 插件")
	}
	name := p.Name()
	if name == "" {
		return errors.New("plugin: 插件名为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; ok {
		return fmt.Errorf("plugin: 插件名 %q 重复注册", name)
	}
	r.byName[name] = p
	r.order = append(r.order, name)
	return nil
}

// Get 按名字取插件。ok == false 表示未注册。
func (r *Registry) Get(name string) (Plugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[name]
	return p, ok
}

// Names 按注册顺序返回插件名。返回副本：调用方改它不影响注册表。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}
