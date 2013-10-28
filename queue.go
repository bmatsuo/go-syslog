package syslog

import (
	"fmt"
)

// not threadsafe. the caller must be theradsafe
type messageQueue struct {
	length int
	head   *message
	tail   *message
}

var errEmpty = fmt.Errorf("empty")
var errHighwater = fmt.Errorf("highwater")

func (q *messageQueue) Dequeue() (*message, error) {
	if q.length == 0 {
		return nil, errEmpty
	}
	msg := q.head
	q.head = q.head.next
	q.length--
	if q.head == nil {
		q.tail = nil
	}
	return msg, nil
}

func (q *messageQueue) Enqueue(highwater int, msg *message) error {
	// check msg first
	if msg == nil {
		return fmt.Errorf("nil msg")
	}
	if msg.next != nil {
		return fmt.Errorf("msg already enqueued")
	}

	if highwater <= q.length {
		return errHighwater
	}

	if q.head == nil {
		q.head = msg
	}
	if q.tail != nil {
		q.tail.next = msg
	}
	q.tail = msg
	q.length++

	return nil
}
