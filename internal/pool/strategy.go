package pool

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	WeightedRandom    = "weighted_random"
	RoundRobin        = "round_robin"
	SequentialExhaust = "sequential_exhaust"
)

// RegionStrategies 使用配置层的地区名；路由层仍使用现有 cn/global realm。
type RegionStrategies struct {
	Domestic      string `json:"domestic"`
	International string `json:"international"`
}

func (s RegionStrategies) Normalize() (RegionStrategies, error) {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"domestic", &s.Domestic}, {"international", &s.International},
	} {
		*field.value = strings.ToLower(strings.TrimSpace(*field.value))
		if *field.value == "" {
			*field.value = WeightedRandom
		}
		switch *field.value {
		case WeightedRandom, RoundRobin, SequentialExhaust:
		default:
			return s, fmt.Errorf("%s: %q 不是合法策略（weighted_random / round_robin / sequential_exhaust）", field.name, *field.value)
		}
	}
	return s, nil
}

// RegionAccountOrder 指定 UID 优先顺序；未列出的账号按 UID 排序追加。
type RegionAccountOrder struct {
	Domestic      []string `json:"domestic,omitempty"`
	International []string `json:"international,omitempty"`
}

type scheduleCursor struct {
	Strategy string `json:"strategy"`
	UID      string `json:"uid"`
}

// SetScheduling 原子热更新两域策略。状态带策略标签，切换算法后忽略旧指针。
func (p *Pool) SetScheduling(s RegionStrategies, order RegionAccountOrder) error {
	s, err := s.Normalize()
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strategies = s
	p.accountOrder = RegionAccountOrder{
		Domestic:      append([]string(nil), order.Domestic...),
		International: append([]string(nil), order.International...),
	}
	for key, cursor := range p.scheduling {
		realm, _, _ := strings.Cut(key, "\x1f")
		if cursor.Strategy != p.strategyForRealmLocked(realm) {
			delete(p.scheduling, key)
			p.dirty.Store(true)
		}
	}
	return nil
}

func (p *Pool) strategyForRealmLocked(realm string) string {
	s := ""
	switch realm {
	case "cn":
		s = p.strategies.Domestic
	case "global":
		s = p.strategies.International
	}
	if s == "" {
		return WeightedRandom
	} // 无 realm 的旧入口不混用地区策略。
	return s
}

// StrategyForRealm 供会话入口决定新会话是否应交由账号池分配。
func (p *Pool) StrategyForRealm(realm string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategyForRealmLocked(realm)
}

func (p *Pool) restoreSchedulingLocked(cursors map[string]scheduleCursor) {
	p.scheduling = make(map[string]scheduleCursor, len(cursors))
	for key, cursor := range cursors {
		realm, _, ok := strings.Cut(key, "\x1f")
		if ok && (realm == "cn" || realm == "global") &&
			(cursor.Strategy == RoundRobin || cursor.Strategy == SequentialExhaust) {
			p.scheduling[key] = cursor
		}
	}
}

// pickOrderedLocked 接收已完成健康、保底、在途与成本层过滤的全量候选。
// 临时 tried/在途占满/成本探索可借用其他账号，但不推进顺序耗尽的主账号。
// 调用方持 p.mu；指针按 (realm, model) 隔离，并通过既有快照机制持久化。
func (p *Pool) pickOrderedLocked(cands []*entry, tried map[string]bool, model, realm, strategy string, explored bool, now time.Time) *entry {
	order := p.accountOrder.Domestic
	if realm == "global" {
		order = p.accountOrder.International
	}
	rank := make(map[string]int, len(order))
	for _, uid := range order {
		if _, exists := rank[uid]; !exists {
			rank[uid] = len(rank)
		}
	}
	lessUID := func(a, b string) bool {
		ar, aok := rank[a]
		br, bok := rank[b]
		if aok != bok {
			return aok
		}
		if aok {
			return ar < br
		}
		return a < b
	}
	sort.Slice(cands, func(i, j int) bool { return lessUID(cands[i].a.UID, cands[j].a.UID) })
	key := realm + "\x1f" + model
	cursor := p.scheduling[key]
	if cursor.Strategy != strategy {
		cursor = scheduleCursor{Strategy: strategy}
	}
	var selected *entry
	if strategy == SequentialExhaust {
		for _, e := range cands {
			if e.a.UID == cursor.UID {
				selected = e
				break
			}
		}
	}
	if selected == nil {
		selected = cands[0]
		if cursor.UID != "" {
			for _, e := range cands {
				if lessUID(cursor.UID, e.a.UID) {
					selected = e
					break
				}
			}
		}
	}
	advance := true
	if strategy == SequentialExhaust && cursor.UID != "" {
		current := p.byUID[cursor.UID]
		if current != nil && current.a.Realm() == realm && current.healthyForModel(now, model) &&
			!p.floorBlockedForRealmModel(current, model, realm, now) &&
			(explored || tried[cursor.UID] || p.inFlightFull(current)) {
			advance = false
		}
	}
	if advance && cursor.UID != selected.a.UID {
		p.scheduling[key] = scheduleCursor{Strategy: strategy, UID: selected.a.UID}
		p.dirty.Store(true)
	}
	return selected
}
