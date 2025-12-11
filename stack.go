package dockerpool

import (
	"sync"
	"sync/atomic"
)

// Stack is a thread-safe LIFO stack.
type Stack[T any] interface {
	Push(elem T)
	Pop() (elem T, ok bool)
	Len() int
}

var _ Stack[any] = (*stack[any])(nil)

type stackNode[T any] struct {
	next *stackNode[T]
	elem T
}

type stack[T any] struct {
	mu sync.Mutex

	length atomic.Int32
	head   *stackNode[T]
}

// NewStack creates a new thread-safe stack.
func NewStack[T any]() Stack[T] {
	return &stack[T]{}
}

func (s *stack[T]) Push(elem T) {
	s.mu.Lock()
	defer s.mu.Unlock()

	node := &stackNode[T]{elem: elem}
	node.next = s.head
	s.head = node
	s.length.Add(1)
}

func (s *stack[T]) Pop() (elem T, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.head == nil {
		return
	}
	elem = s.head.elem
	s.head = s.head.next
	s.length.Add(-1)

	return elem, true
}

func (s *stack[T]) Len() int {
	return int(s.length.Load())
}
