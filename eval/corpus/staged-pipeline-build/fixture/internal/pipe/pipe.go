// Package pipe implements a staged text-processing pipeline: a list of
// Stage functions applied in order to a token slice.
package pipe

import "strings"

// Config controls optional pipeline behavior.
type Config struct {
	// MinLen drops tokens shorter than this during Run; 0 disables.
	MinLen int
	// Reverse reverses the final token order when set.
	Reverse bool
}

// Stage transforms a token slice.
type Stage func([]string) []string

// Pipeline applies its stages in order.
type Pipeline struct {
	cfg    Config
	stages []Stage
}

// New returns a Pipeline with the default stage chain: normalize,
// tokenize.
func New(cfg Config) *Pipeline {
	return &Pipeline{
		cfg: cfg,
		stages: []Stage{
			normalize,
			tokenize,
		},
	}
}

// normalize lowercases each token. It is intended to also trim
// surrounding whitespace.
func normalize(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, strings.ToLower(t))
	}
	return out
}

// tokenize splits each element on whitespace.
func tokenize(tokens []string) []string {
	var out []string
	for _, t := range tokens {
		out = append(out, strings.Fields(t)...)
	}
	return out
}

// Run applies every stage in order. Config options are declared but
// not all are wired yet.
func (p *Pipeline) Run(input []string) []string {
	out := input
	for _, s := range p.stages {
		out = s(out)
	}
	return out
}
