// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"math"
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在候选集中的最大虚拟实例权重（倍数上限）。
// 实际倍数按「快过期剩余量占候选集最大值的比例」从 1 线性升到本值（见
// expiringMultiplier / expiringSlots）——快过期越多越接近满额，不再「有就 ×3」。
const expiringVirtualSlots = 3

// expiringSlots 把快过期加权倍数取整成虚拟实例数（1 ~ expiringVirtualSlots），
// 供新会话候选集按剩余量成比例加权：剩余量最大者取满 expiringVirtualSlots，其余按
// expiringMultiplier 四舍五入（至少 1 次，保持普通账号的基线出现次数）。
func expiringSlots(expiring, maxExpiring int64) int {
	slots := int(math.Round(expiringMultiplier(expiring, maxExpiring)))
	if slots < 1 {
		return 1
	}
	if slots > expiringVirtualSlots {
		return expiringVirtualSlots
	}
	return slots
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// ServableForModelRealm 返回 realm 内此刻**真正能服务该模型**的账号数。
//
// 与 pick 的候选口径一致（healthyForModel 的 6004/11102 模型级冷却豁免、在途租约、
// 积分保底都计入），但**不含 pick 的全冷却兜底**——跨域回退（issue #135）要的正是
// 「这个域此刻有没有健康号」，兜底把冷却号捞回来的乐观语义会掩盖这一点。
//
// 只读：持 RLock 遍历，不改任何状态。realm=="" 时统计全池（与既有 realm 语义一致）。
func (p *Pool) ServableForModelRealm(model, realm string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue
		}
		n++
	}
	return n
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号按「快过期剩余量占候选集最大值的比例」出现
// 1 ~ expiringVirtualSlots 次（剩余最多者取满）。调用方继续按原有序列表哈希，即可
// 让新会话对快过期积分多的账号形成更强偏好。重复项按 UID 排序后展开，保证同一
// 账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	var maxExpiring int64
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
		if p.preferExpiring && expiringNow(e, now) && e.creditsExpiring > maxExpiring {
			maxExpiring = e.creditsExpiring
		}
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringSlots(e.creditsExpiring, maxExpiring)
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
