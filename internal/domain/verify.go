package domain

import "time"

// VerifyKey is the reserved key name the calls of targets verify are recorded
// under (RSN-9): no key name may start with "_".
const VerifyKey = "_verify"

// VerifyCall is one call of a verify run: the level asked ("" for the call
// without reasoning control), what Build sent and what the provider answered.
type VerifyCall struct {
	Effort          string  `json:"effort"`
	EffortSent      *string `json:"effort_sent"`
	Status          int     `json:"status"` // the provider's; 0 without a response
	ReasoningTokens *int64  `json:"reasoning_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	Body            string  `json:"body"` // the exact bytes sent (PAR-8)
	RequestID       string  `json:"request_id"`
	Error           *Error  `json:"error,omitempty"`
	// ProviderMessage is the provider's text as the attempt's log line
	// carries it, redacted and cut, wherever the error does not: the reason a
	// provider gave for refusing the operator's own call.
	ProviderMessage string `json:"provider_message,omitempty"`
}

// Verification is the result of one verify run (RSN-9). It lives here, not in
// api, because the admin listener carries it and api's tests import admin.
type Verification struct {
	Target   string       `json:"target"`
	Version  string       `json:"version"` // the proxy version that ran it
	OK       bool         `json:"ok"`      // every call answered 2xx
	At       time.Time    `json:"at"`      // the service clock at the end of the run
	Calls    []VerifyCall `json:"calls"`
	Spec     Target       `json:"-"` // what was verified, for config.Service.Verified
	Provider Provider     `json:"-"`
}

// VerifyLevels are the levels a verify run asks of a target, one call each
// (RSN-9): every declared level in order, then none when can_disable is true;
// with neither, one call without reasoning control, "".
func VerifyLevels(r *Reasoning) []string {
	var levels []string
	if r != nil {
		levels = append(levels, r.Levels...)
		if r.CanDisable != nil && *r.CanDisable {
			levels = append(levels, "none")
		}
	}
	if len(levels) == 0 {
		return []string{""}
	}
	return levels
}
