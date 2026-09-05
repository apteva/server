package main

import (
	"context"
	"sync"
	"syscall"
	"time"
)

func (im *AgentManager) CachedCoreRuntimeInfo(id int64) (coreRuntimeInfo, bool) {
	im.mu.RLock()
	ri := im.processes[id]
	im.mu.RUnlock()
	if ri == nil || !ri.isRunning() {
		return coreRuntimeInfo{}, false
	}
	ri.runtimeMu.Lock()
	defer ri.runtimeMu.Unlock()
	if ri.runtimeAt.IsZero() {
		return coreRuntimeInfo{}, false
	}
	info := ri.runtimeInfo
	info.UptimeSeconds += int(time.Since(ri.runtimeAt).Seconds())
	return info, true
}

// RunRuntimeMonitor refreshes observations independently of dashboard traffic.
// Starts synchronously seed the snapshot through their existing health check.
func (s *Server) RunRuntimeMonitor(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.agents.mu.RLock()
		ids := make([]int64, 0, len(s.agents.processes))
		for id := range s.agents.processes {
			ids = append(ids, id)
		}
		s.agents.mu.RUnlock()
		jobs := make(chan int64)
		var wg sync.WaitGroup
		for i := 0; i < min(16, len(ids)); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for id := range jobs {
					if ctx.Err() != nil {
						continue
					}
					s.agents.mu.Lock()
					ri := s.agents.processes[id]
					if ri != nil && ri.reattached && ri.pid > 0 && syscall.Kill(ri.pid, 0) == syscall.ESRCH {
						delete(s.agents.processes, id)
						s.agents.mu.Unlock()
						// Conditional write cannot clear a concurrently published replacement.
						s.store.db.Exec("UPDATE agents SET status='stopped',pid=0,port=0,core_api_key='',core_started_at=NULL WHERE id=? AND pid=? AND port=?", id, ri.pid, ri.port)
						continue
					}
					s.agents.mu.Unlock()
					s.agents.coreRuntimeInfoContext(ctx, id)
				}
			}()
		}
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
			}
		}
		close(jobs)
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
