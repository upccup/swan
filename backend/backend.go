package backend

import (
	"github.com/Dataman-Cloud/swan/scheduler"
	"github.com/Dataman-Cloud/swan/store"
)

type Backend struct {
	sched *scheduler.Scheduler
	store store.Store
}

func NewBackend(sched *scheduler.Scheduler, store store.Store) *Backend {
	return &Backend{
		sched: sched,
		store: store,
	}
}

func (b *Backend) ClusterId() string {
	return b.sched.ClusterId
}
