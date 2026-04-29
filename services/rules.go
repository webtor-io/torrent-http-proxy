package services

import "net/http"

// responseRuleHandler processes an HTTP response according to a specific rule
// kind/scope. Each handler self-gates against rc.Claims and the response
// (path, content-type, etc.) and is a no-op when its rule isn't present.
type responseRuleHandler func(r *http.Response, rc *RulesContext) error

// responseRuleHandlers is the registered chain run from modifyResponse via
// applyResponseRules. New rule kinds add themselves here instead of touching
// the proxy hook directly.
var responseRuleHandlers = []responseRuleHandler{
	rewriteManifestForGrace,
}

// applyResponseRules is the single entry point for rule-driven response
// processing. It loads the per-request RulesContext (set in web.go proxyHTTP)
// and runs every registered handler. Stays kind-agnostic — dispatch decisions
// live inside handlers.
func applyResponseRules(r *http.Response) error {
	if r.Request == nil {
		return nil
	}
	rc := GetRulesContext(r.Request)
	if rc == nil || rc.Claims == nil {
		return nil
	}
	for _, h := range responseRuleHandlers {
		if err := h(r, rc); err != nil {
			return err
		}
	}
	return nil
}
