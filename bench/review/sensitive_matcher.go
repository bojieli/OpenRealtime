package review

import (
	"context"
	"errors"
	"fmt"
	"math"
)

const maximumSensitiveAutomatonCells = 17 << 20

type sensitiveMatcher struct {
	alphabet [256]int16
	width    int
	next     []uint32
	matched  []bool
}

type sensitiveTrieNode struct {
	next    map[byte]int
	failure int
	matched bool
}

// newSensitiveMatcher compiles all declared values into one deterministic
// Aho-Corasick automaton. Matching is linear in the payload rather than one
// full payload scan per secret. The aggregate input and dense transition table
// are both hard bounded before allocation.
func newSensitiveMatcher(values [][]byte) (*sensitiveMatcher, error) {
	return newSensitiveMatcherContext(context.Background(), values)
}

func newSensitiveMatcherContext(ctx context.Context, values [][]byte) (*sensitiveMatcher, error) {
	if ctx == nil {
		return nil, errors.New("review sensitive-value matcher requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matcher := &sensitiveMatcher{}
	for index := range matcher.alphabet {
		matcher.alphabet[index] = -1
	}
	if len(values) == 0 {
		return matcher, nil
	}
	total := 0
	var used [256]bool
	nodes := []sensitiveTrieNode{{next: make(map[byte]int)}}
	for index, value := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(value) == 0 || total > maximumSensitiveAggregateBytes-len(value) {
			return nil, fmt.Errorf("review sensitive values exceed the %d-byte aggregate limit at value %d",
				maximumSensitiveAggregateBytes, index)
		}
		total += len(value)
		state := 0
		for offset, character := range value {
			if offset&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			used[character] = true
			next, exists := nodes[state].next[character]
			if !exists {
				next = len(nodes)
				nodes[state].next[character] = next
				nodes = append(nodes, sensitiveTrieNode{next: make(map[byte]int)})
			}
			state = next
		}
		nodes[state].matched = true
	}
	for character := 0; character < len(used); character++ {
		if used[character] {
			matcher.alphabet[character] = int16(matcher.width)
			matcher.width++
		}
	}
	if matcher.width <= 0 || len(nodes) > math.MaxInt/matcher.width ||
		len(nodes)*matcher.width > maximumSensitiveAutomatonCells {
		return nil, errors.New("review sensitive-value matcher exceeds its structural bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matcher.next = make([]uint32, len(nodes)*matcher.width)
	matcher.matched = make([]bool, len(nodes))
	for state := range nodes {
		if state&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		matcher.matched[state] = nodes[state].matched
		for character, next := range nodes[state].next {
			matcher.next[state*matcher.width+int(matcher.alphabet[character])] = uint32(next)
		}
	}
	queue := make([]int, 0, len(nodes)-1)
	for _, child := range nodes[0].next {
		queue = append(queue, child)
	}
	for cursor := 0; cursor < len(queue); cursor++ {
		if cursor&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		state := queue[cursor]
		failure := nodes[state].failure
		for character := 0; character < len(used); character++ {
			column := matcher.alphabet[character]
			if column < 0 {
				continue
			}
			position := state*matcher.width + int(column)
			if child, direct := nodes[state].next[byte(character)]; direct {
				nodes[child].failure = int(matcher.next[failure*matcher.width+int(column)])
				matcher.matched[child] = matcher.matched[child] || matcher.matched[nodes[child].failure]
				queue = append(queue, child)
				continue
			}
			matcher.next[position] = matcher.next[failure*matcher.width+int(column)]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return matcher, nil
}

func (matcher *sensitiveMatcher) contains(payload []byte) bool {
	matched, _ := matcher.containsContext(context.Background(), payload)
	return matched
}

func (matcher *sensitiveMatcher) containsContext(
	ctx context.Context, payload []byte,
) (bool, error) {
	if matcher == nil || matcher.width == 0 || len(payload) == 0 {
		if ctx != nil {
			return false, ctx.Err()
		}
		return false, errors.New("review sensitive-value scan requires a context")
	}
	state := uint32(0)
	return matcher.advanceContext(ctx, payload, &state)
}

// advance continues one match across an ordered sequence of separately
// decoded fields. Security boundaries use it to reject a secret split across
// adjacent JSON string tokens rather than treating token boundaries as an
// exfiltration escape hatch.
func (matcher *sensitiveMatcher) advance(payload []byte, state *uint32) bool {
	matched, _ := matcher.advanceContext(context.Background(), payload, state)
	return matched
}

func (matcher *sensitiveMatcher) advanceContext(
	ctx context.Context, payload []byte, state *uint32,
) (bool, error) {
	if ctx == nil {
		return false, errors.New("review sensitive-value scan requires a context")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if matcher == nil || matcher.width == 0 || len(payload) == 0 || state == nil {
		return false, nil
	}
	for index, character := range payload {
		if index&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		column := matcher.alphabet[character]
		if column < 0 {
			*state = 0
			continue
		}
		*state = matcher.next[int(*state)*matcher.width+int(column)]
		if matcher.matched[*state] {
			return true, nil
		}
	}
	return false, ctx.Err()
}
