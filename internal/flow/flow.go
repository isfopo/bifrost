// Package flow compiles a config flow's JSONata expressions once at startup
// and evaluates them per webhook: when (route filter) and map (transform).
package flow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	jsonata "github.com/blues/jsonata-go"

	"bifrost/internal/config"
)

// Compiled is a config flow with its JSONata expressions compiled once at
// startup so the hot path only evaluates.
type Compiled struct {
	Name    string
	cfg     *config.Flow
	when    *jsonata.Expr // nil = always route
	mapExpr *jsonata.Expr
}

// Config exposes the underlying flow configuration.
func (cf *Compiled) Config() *config.Flow {
	return cf.cfg
}

// CompileFlows compiles every flow's JSONata expressions. Startup fails fast
// on syntax errors so a bad mapping can't be silently half-deployed.
func CompileFlows(cfg *config.Config) (map[string]*Compiled, error) {
	out := make(map[string]*Compiled, len(cfg.Flows))
	for name, f := range cfg.Flows {
		cf := &Compiled{Name: name, cfg: f}
		if strings.TrimSpace(f.When) != "" {
			e, err := jsonata.Compile(f.When)
			if err != nil {
				return nil, fmt.Errorf("flow %q: 'when' expression: %w", name, err)
			}
			cf.when = e
		}
		if strings.TrimSpace(f.Map) == "" {
			return nil, fmt.Errorf("flow %q: 'map' expression is required", name)
		}
		e, err := jsonata.Compile(f.Map)
		if err != nil {
			return nil, fmt.Errorf("flow %q: 'map' expression: %w", name, err)
		}
		cf.mapExpr = e
		out[name] = cf
	}
	return out, nil
}

// Matches evaluates the flow's `when` filter against the parsed event body.
// A flow without a `when` matches everything.
func (cf *Compiled) Matches(doc any) (bool, error) {
	if cf.when == nil {
		return true, nil
	}
	v, err := cf.when.Eval(doc)
	if err != nil {
		// "no match" (undefined result) is a normal routing outcome; anything
		// else is an expression error worth surfacing.
		if errors.Is(err, jsonata.ErrUndefined) {
			return false, nil
		}
		return false, fmt.Errorf("'when' expression: %w", err)
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("'when' expression must return a boolean, got %T", v)
	}
	return b, nil
}

// MapBody evaluates the flow's `map` transform and validates the result is
// usable as an outgoing body: either an object (map) or an array.
func (cf *Compiled) MapBody(doc any) (any, error) {
	v, err := cf.mapExpr.Eval(doc)
	if err != nil {
		return nil, fmt.Errorf("map expression: %w", err)
	}
	switch v.(type) {
	case map[string]any, []any:
		return v, nil
	default:
		return nil, fmt.Errorf("map expression must produce a JSON object or array, got %T", v)
	}
}

// ResolveEndpoint returns the endpoint string. Endpoints containing "$" are
// evaluated as JSONata against the mapped output (e.g. putting the CalDAV
// file name from the payload's uid); anything else passes through literally.
func ResolveEndpoint(endpoint string, mapped any) (string, error) {
	if !strings.Contains(endpoint, "$") {
		return endpoint, nil
	}
	e, err := jsonata.Compile(endpoint)
	if err != nil {
		return "", fmt.Errorf("endpoint expression: %w", err)
	}
	v, err := e.Eval(mapped)
	if err != nil {
		return "", fmt.Errorf("endpoint expression: %w", err)
	}
	s, err := asString(v)
	if err != nil {
		return "", err
	}
	return s, nil
}

func asString(v any) (string, error) {
	switch s := v.(type) {
	case string:
		return s, nil
	case nil:
		return "", fmt.Errorf("endpoint expression produced no value")
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("endpoint expression produced non-string: %w", err)
		}
		return string(b), nil
	}
}
