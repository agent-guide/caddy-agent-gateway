package provider

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

type CandidateIdentity struct {
	ProviderID    string
	ProviderType  string
	UpstreamModel string
}

type RequirementGap struct {
	Candidate CandidateIdentity
	Missing   []ProtocolFeature
}

func (g *RequirementGap) Error() string {
	if g == nil {
		return "protocol requirement gap"
	}
	names := make([]string, len(g.Missing))
	for i, feature := range g.Missing {
		names[i] = string(feature)
	}
	return fmt.Sprintf(
		"candidate provider_id=%q provider_type=%q upstream_model=%q is missing protocol features: %s",
		g.Candidate.ProviderID,
		g.Candidate.ProviderType,
		g.Candidate.UpstreamModel,
		strings.Join(names, ", "),
	)
}

func (*RequirementGap) StatusCode() int { return http.StatusNotImplemented }

type RequirementGapsError struct {
	Target string
	Gaps   []RequirementGap
}

func (e *RequirementGapsError) Error() string {
	features := map[ProtocolFeature]struct{}{}
	for _, gap := range e.Gaps {
		for _, feature := range gap.Missing {
			features[feature] = struct{}{}
		}
	}
	names := make([]string, 0, len(features))
	for feature := range features {
		names = append(names, string(feature))
	}
	sort.Strings(names)
	return fmt.Sprintf("model target %q has no eligible bindings; missing protocol features: %s", e.Target, strings.Join(names, ", "))
}

func (*RequirementGapsError) StatusCode() int { return http.StatusBadGateway }
