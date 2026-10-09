package engine

import (
	"go.uber.org/zap"

	mylogger "github.com/taha2samy/hypergate/internal/logger"
)

// ChainExecutor is responsible for executing a chain of filters against a request context.
type ChainExecutor struct{}

// NewChainExecutor creates a new ChainExecutor.
func NewChainExecutor() *ChainExecutor {
	return &ChainExecutor{}
}

// Execute processes the RequestContext through the provided filter chain for a specific phase.
// Filters that implement PhaseAware are skipped if the current phase is not in their declared
// set. Filters that do NOT implement PhaseAware run only on PhaseRequestHeaders (legacy default).
// The executor fast-fails immediately if the context becomes blocked.
func (e *ChainExecutor) Execute(ctx *RequestContext, chain Chain, phase Phase) error {
	mylogger.Debug("Executing filter chain", zap.Int("filters_count", len(chain)), zap.Uint8("phase", uint8(phase)))
	ctx.Phase = phase

	for i := 0; i < len(chain); i++ {
		// Fast-Fail / Circuit Break if previously blocked
		if ctx.Blocked {
			mylogger.Info("Request blocked by filter chain",
				zap.Int32("status_code", int32(ctx.ResponseStatus)),
				zap.String("response_body", ctx.ResponseBody),
				zap.String("path", ctx.Path),
			)
			break
		}

		filter, audited := isAudited(chain[i])

		// Phase gating: only call the filter if it declared support for this phase.
		// Filters without PhaseAware default to PhaseRequestHeaders only.
		if pa, ok := filter.(PhaseAware); ok {
			if !phaseSupported(pa.SupportedPhases(), phase) {
				continue
			}
		} else if phase != PhaseRequestHeaders {
			// Legacy filter: skip on every phase except the first
			continue
		}

		mylogger.Debug("Executing filter in chain", zap.Int("filter_index", i), zap.Uint8("phase", uint8(phase)))

		if audited {
			e.executeAudited(ctx, filter, i)
			continue
		}

		if err := filter.Execute(ctx); err != nil {
			mylogger.Error("Filter execution failed with internal error", zap.String("error", err.Error()), zap.Int("filter_index", i))

			// Block the request gracefully due to internal server error
			ctx.BlockedBy = i
			ctx.FilterFailed = true
			ctx.Blocked = true
			ctx.ResponseStatus = 500
			ctx.ResponseBody = "Internal Server Error"

			return err
		}

		// Fast-Fail after filter execution
		if ctx.Blocked {
			ctx.BlockedBy = i
			mylogger.Info("Request blocked by filter chain",
				zap.Int32("status_code", int32(ctx.ResponseStatus)),
				zap.String("response_body", ctx.ResponseBody),
				zap.String("path", ctx.Path),
			)
			break
		}
	}

	return nil
}

// executeAudited runs a filter in audit mode: a denial or an internal error is
// recorded in ctx.AuditHits, the filter's header changes are rolled back and
// the chain continues. Answers (such as a CORS preflight) are still sent.
func (e *ChainExecutor) executeAudited(ctx *RequestContext, filter Filter, index int) {
	saved := ctx.saveMutations()
	err := filter.Execute(ctx)
	switch {
	case err != nil:
		mylogger.Info("Audited filter failed; not enforced", zap.Int("filter_index", index), zap.String("error", err.Error()))
		ctx.AuditHits = append(ctx.AuditHits, AuditHit{Index: index, Status: 500, Failed: true})
	case ctx.Blocked && !ctx.Answered:
		status := ctx.ResponseStatus
		if status == 0 {
			status = 403
		}
		mylogger.Info("Audited filter would deny; not enforced",
			zap.Int("filter_index", index), zap.Int32("status_code", status), zap.String("path", ctx.Path))
		ctx.AuditHits = append(ctx.AuditHits, AuditHit{Index: index, Status: status})
	case ctx.Blocked && ctx.Answered:
		ctx.BlockedBy = index
		return
	default:
		return
	}
	ctx.restoreMutations(saved)
	ctx.unblock()
}

// phaseSupported is a tight inner loop check — avoids map allocation.
func phaseSupported(phases []Phase, target Phase) bool {
	for _, p := range phases {
		if p == target {
			return true
		}
	}
	return false
}
