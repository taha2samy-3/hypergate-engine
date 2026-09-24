package correlation_id

import (
	"regexp"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/rs/xid"

	"github.com/taha2samy/hypergate/internal/engine"
)

func init() {
	uuid.EnableRandPool()
}

type CorrelationConfig struct {
	HeaderName              string         `yaml:"header_name"`
	Algorithm               string         `yaml:"algorithm"`
	Mode                    string         `yaml:"mode"`
	Prefix                  string         `yaml:"prefix"`
	PropagateToUpstream     bool           `yaml:"propagate_to_upstream"`
	PropagateToDownstream   bool           `yaml:"propagate_to_downstream"`
	InputHeaderName         string         `yaml:"input_header_name"`
	ResponseHeaderName      string         `yaml:"response_header_name"`
	ValidationRegex         string         `yaml:"validation_regex"`
	CompiledValidationRegex *regexp.Regexp `yaml:"-"`
}

type CorrelationFilter struct {
	config CorrelationConfig
}

func NewCorrelationFilter(cfg CorrelationConfig) (*CorrelationFilter, error) {
	if cfg.HeaderName == "" {
		cfg.HeaderName = "x-request-id"
	}
	if cfg.Algorithm == "" {
		cfg.Algorithm = "uuidv4"
	}
	if cfg.Mode == "" {
		cfg.Mode = "if_missing"
	}
	if cfg.InputHeaderName == "" {
		cfg.InputHeaderName = cfg.HeaderName
	}
	if cfg.ResponseHeaderName == "" {
		cfg.ResponseHeaderName = cfg.HeaderName
	}
	if cfg.ValidationRegex != "" {
		re, err := regexp.Compile(cfg.ValidationRegex)
		if err != nil {
			return nil, err
		}
		cfg.CompiledValidationRegex = re
	}
	return &CorrelationFilter{config: cfg}, nil
}

func (f *CorrelationFilter) Execute(ctx *engine.RequestContext) error {
	incomingID := ctx.GetHeader(f.config.InputHeaderName)
	generate := false

	if incomingID == "" {
		generate = true
	} else if f.config.Mode == "overwrite" {
		generate = true
	} else if f.config.CompiledValidationRegex != nil && !f.config.CompiledValidationRegex.MatchString(incomingID) {
		generate = true
	}

	var finalID string
	if generate {
		switch f.config.Algorithm {
		case "uuidv7":
			u, _ := uuid.NewV7()
			finalID = f.config.Prefix + u.String()
		case "xid":
			finalID = f.config.Prefix + xid.New().String()
		case "ulid":
			finalID = f.config.Prefix + ulid.Make().String()
		case "uuidv4":
			fallthrough
		default:
			finalID = f.config.Prefix + uuid.NewString()
		}
	} else {
		finalID = incomingID
	}

	if f.config.PropagateToUpstream {
		ctx.SetHeaderUpstream(f.config.HeaderName, finalID)
	}
	if f.config.PropagateToDownstream {
		ctx.SetHeaderDownstream(f.config.ResponseHeaderName, finalID)
	}

	return nil
}
