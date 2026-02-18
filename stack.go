package dockerpool

import (
	"sync"
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
	rw sync.RWMutex

	length int
	head   *stackNode[T]
}

// NewStack creates a new thread-safe stack.
func NewStack[T any]() Stack[T] {
	return &stack[T]{}
}

func (s *stack[T]) Push(elem T) {
	s.rw.Lock()
	defer s.rw.Unlock()

	node := &stackNode[T]{elem: elem}
	node.next = s.head
	s.head = node
	s.length += 1
}

func (s *stack[T]) Pop() (elem T, ok bool) {
	s.rw.Lock()
	defer s.rw.Unlock()

	if s.head == nil {
		return
	}
	elem = s.head.elem
	s.head = s.head.next
	s.length += -1

	return elem, true
}

func (s *stack[T]) Len() int {
	s.rw.RLock()
	defer s.rw.RUnlock()

	return s.length
}
