package dockerpool

import (
	"sync"
	"sync/atomic"
)

type Pool[T any] interface {
	Push(elem T)
	Pop() (elem T, ok bool)
	Len() int
}

var _ Pool[any] = (*pool[any])(nil)

type poolNode[T any] struct {
	next *poolNode[T]
	elem T
}
type pool[T any] struct {
	mu sync.Mutex

	length atomic.Int32
	head   *poolNode[T]
}

func NewPool[T any]() Pool[T] {
	return &pool[T]{}
}

func (p *pool[T]) Push(elem T) {
	p.mu.Lock()
	defer p.mu.Unlock()

	node := &poolNode[T]{elem: elem}
	node.next = p.head
	p.head = node
	p.length.Add(1)
}

func (p *pool[T]) Pop() (elem T, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.head == nil {
		return
	}
	elem = p.head.elem
	p.head = p.head.next
	p.length.Add(-1)

	return elem, true
}

func (p *pool[T]) Len() int {
	return int(p.length.Load())
}
