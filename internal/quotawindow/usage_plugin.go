package quotawindow

import (
	"context"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// UsagePlugin settles token dimensions against the window captured at admission.
type UsagePlugin struct {
	mu      sync.Mutex
	pending map[string]coreusage.Detail
}

func (g *Gate) UsagePlugin() coreusage.Plugin {
	if g == nil {
		return nil
	}
	return &UsagePlugin{pending: make(map[string]coreusage.Detail)}
}

// SettleQuotaWindowReservation applies final usage to an admitted attempt.
func (g *Gate) SettleQuotaWindowReservation(reservation string, detail coreusage.Detail) {
	if g == nil || g.ledger == nil {
		return
	}
	g.admissionMu.Lock()
	defer g.admissionMu.Unlock()
	g.ledger.Settle(reservation, detail)
}

func (p *UsagePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil {
		return
	}
	reservation := coreauth.QuotaWindowReservationFromContext(ctx)
	if reservation == "" {
		return
	}
	if record.AdditionalUsagePending {
		p.mu.Lock()
		if p.pending == nil {
			p.pending = make(map[string]coreusage.Detail)
		}
		p.pending[reservation] = record.Detail
		p.mu.Unlock()
		return
	}
	detail := record.Detail
	if record.AdditionalUsage {
		p.mu.Lock()
		primary, exists := p.pending[reservation]
		delete(p.pending, reservation)
		p.mu.Unlock()
		if exists {
			detail.InputTokens = saturatingUsageAdd(primary.InputTokens, detail.InputTokens)
			detail.OutputTokens = saturatingUsageAdd(primary.OutputTokens, detail.OutputTokens)
			detail.TotalTokens = saturatingUsageAdd(primary.TotalTokens, detail.TotalTokens)
		}
	}
	coreauth.SettleQuotaWindowUpstreamAttempt(ctx, detail)
}
