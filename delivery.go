package watcher

import "sync"

type lineDelivery struct {
	generation   uint64
	acknowledged bool
	commit       func() error
}

type deliveryQueue struct {
	mu         sync.Mutex
	generation uint64
	lines      []*lineDelivery
}

func (q *deliveryQueue) add(generation uint64, commit func() error) func() error {
	q.mu.Lock()
	if generation != q.generation {
		q.generation = generation
		q.lines = nil
	}
	line := &lineDelivery{generation: generation, commit: commit}
	q.lines = append(q.lines, line)
	q.mu.Unlock()
	return func() error {
		q.mu.Lock()
		defer q.mu.Unlock()
		if line.generation != q.generation {
			return nil
		}
		line.acknowledged = true
		for len(q.lines) > 0 && q.lines[0].acknowledged {
			if err := q.lines[0].commit(); err != nil {
				return err
			}
			q.lines[0] = nil
			q.lines = q.lines[1:]
		}
		return nil
	}
}

type transcriptBatch struct {
	offset      int64
	generation  uint64
	events      []Event
	next        int
	acknowledge func() error
}
