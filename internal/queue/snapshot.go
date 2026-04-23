package queue

// Snapshot returns a point-in-time, deep-copied view of all jobs in the
// queue. It is intended for testing and consistent-read views (e.g. for
// API responses).
//
// The returned slice and the Jobs within it are fresh allocations; mutations
// to the returned objects do not affect the Queue's internal state.
func (q *Queue) Snapshot() []*Job {
	q.mu.RLock()
	defer q.mu.RUnlock()

	res := make([]*Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		res = append(res, cloneJob(j))
	}
	return res
}

func cloneJob(j *Job) *Job {
	cp := *j

	// Deep copy maps
	if j.ServerStats != nil {
		cp.ServerStats = make(map[string]int64, len(j.ServerStats))
		for k, v := range j.ServerStats {
			cp.ServerStats[k] = v
		}
	}
	if j.Meta != nil {
		cp.Meta = make(map[string][]string, len(j.Meta))
		for k, v := range j.Meta {
			vCp := make([]string, len(v))
			copy(vCp, v)
			cp.Meta[k] = vCp
		}
	}

	// Deep copy slices
	if j.Groups != nil {
		cp.Groups = make([]string, len(j.Groups))
		copy(cp.Groups, j.Groups)
	}

	if j.Files != nil {
		cp.Files = make([]JobFile, len(j.Files))
		for i, f := range j.Files {
			fCp := f
			if f.Articles != nil {
				fCp.Articles = make([]JobArticle, len(f.Articles))
				copy(fCp.Articles, f.Articles)
			}
			cp.Files[i] = fCp
		}
	}

	return &cp
}

// SnapshotJob returns a point-in-time, deep-copied view of a single job
// by ID. Returns nil if the job is not found.
func (q *Queue) SnapshotJob(id string) *Job {
	q.mu.RLock()
	defer q.mu.RUnlock()

	j, ok := q.byID[id]
	if !ok {
		return nil
	}
	return cloneJob(j)
}

// SnapshotJobByName returns a point-in-time, deep-copied view of a single
// job by its human-readable name. Returns nil if the job is not found.
func (q *Queue) SnapshotJobByName(name string) *Job {
	q.mu.RLock()
	defer q.mu.RUnlock()

	for _, j := range q.jobs {
		if j.Name == name {
			return cloneJob(j)
		}
	}
	return nil
}
