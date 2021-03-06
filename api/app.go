package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Dataman-Cloud/swan/mesos"
	"github.com/Dataman-Cloud/swan/types"
	"github.com/Dataman-Cloud/swan/utils"
	"github.com/Dataman-Cloud/swan/utils/fields"
	"github.com/Dataman-Cloud/swan/utils/labels"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
)

func (r *Server) createApp(w http.ResponseWriter, req *http.Request) {
	if err := checkForJSON(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var version types.Version
	if err := decode(req.Body, &version); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := version.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	version.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())

	compose := req.Form.Get("compose")

	if compose == "" {
		compose = "default"
	}

	// ensure proxy Listen & Alias uniq
	if err := r.checkProxyDuplication(version.Proxy); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// ensure os ports not in using
	if err := r.checkPortListening(version.Proxy); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	cluster := r.driver.ClusterName()
	if c := utils.StripSpaces(version.Cluster); c != "" {
		cluster = c
	}

	var (
		id    = fmt.Sprintf("%s.%s.%s.%s", version.Name, compose, version.RunAs, cluster)
		count = int(version.Instances)
	)

	app := &types.Application{
		ID:        id,
		Name:      version.Name,
		RunAs:     version.RunAs,
		Cluster:   cluster,
		OpStatus:  types.OpStatusCreating,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := r.db.CreateApp(app); err != nil {
		if strings.Contains(err.Error(), "app already exists") {
			http.Error(w, fmt.Sprintf("app %s has already exists", id), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("create app error: %v", err), http.StatusInternalServerError)
		return
	}

	if err := r.db.CreateVersion(id, &version); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	go func(appId string) {
		var err error

		// defer to mark op status
		defer func() {
			if err != nil {
				log.Errorf("launch app %s error: %v", appId, err)
				r.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("launch app error: %v", err))
			} else {
				log.Printf("launch app %s succeed", appId)
				r.memoAppStatus(appId, types.OpStatusNoop, "")
			}
		}()

		// prepare for all runtime tasks & db tasks
		log.Printf("Preparing to launch App %s with %d tasks", id, count)

		tasks := []*mesos.Task{}
		for i := 0; i < count; i++ {
			var (
				name = fmt.Sprintf("%d.%s", i, appId)
				id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
			)

			// runtime tasks
			cfg := types.NewTaskConfig(&version, i)
			t := mesos.NewTask(cfg, id, name)
			tasks = append(tasks, t)
		}

		err = r.driver.LaunchTasks(tasks)
		if err != nil {
			err = fmt.Errorf("launch tasks got error: %v", err)
			return
		}
	}(app.ID)

	writeJSON(w, http.StatusCreated, map[string]string{"id": app.ID})
}

func (r *Server) listApps(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	filters := types.AppFilterOptions{}

	labelsFilter := req.Form.Get("labels")
	if labelsFilter != "" {
		selector, err := labels.Parse(labelsFilter)
		if err != nil {
			http.Error(w, fmt.Sprintf("parse labels %s failed: %v", selector, err), http.StatusBadRequest)
			return
		}
		filters.LabelsSelector = selector
	}

	fieldsFilter := req.Form.Get("fields")
	if fieldsFilter != "" {
		selector, err := fields.ParseSelector(fieldsFilter)
		if err != nil {
			http.Error(w, fmt.Sprintf("parse labels %s failed: %v", selector, err), http.StatusBadRequest)
			return
		}

		filters.FieldsSelector = selector
	}

	rets, err := r.db.ListApps()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	apps := make([]*types.Application, 0)
	for _, app := range rets {
		ver, err := r.db.GetVersion(app.ID, app.Version[0])
		if err != nil {
			if r.db.IsErrNotFound(err) {
				if filters.LabelsSelector == nil && filters.FieldsSelector == nil {
					apps = append(apps, app)
				}
				continue
			}

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if filters.LabelsSelector != nil {
			if !filterByLabelsSelectors(filters.LabelsSelector, ver.Labels) {
				continue
			}
		}

		if filters.FieldsSelector != nil {
			if !filterByFieldsSelectors(filters.FieldsSelector, ver) {
				continue
			}
		}

		apps = append(apps, app)
	}

	writeJSON(w, http.StatusOK, apps)
}

func (r *Server) getApp(w http.ResponseWriter, req *http.Request) {
	// TODO(nmg): mux.Vars should be wrapped in context.
	id := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(id)
	if err != nil {
		if r.db.IsErrNotFound(err) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, app)
}

func (r *Server) deleteApp(w http.ResponseWriter, req *http.Request) {
	var (
		appId = mux.Vars(req)["app_id"]
	)

	// get app
	_, err := r.db.GetApp(appId)
	if err != nil {
		if r.db.IsErrNotFound(err) {
			http.Error(w, fmt.Sprintf("app %s not exists", appId), http.StatusNotFound)
			return
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// get app tasks
	tasks, err := r.db.ListTasks(appId)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for delete app. %v", err), http.StatusInternalServerError)
		return
	}

	log.Debugf("app %s has %d tasks", appId, len(tasks))

	// get app versions
	versions, err := r.db.ListVersions(appId)
	if err != nil && !r.db.IsErrNotFound(err) {
		http.Error(w, fmt.Sprintf("list tasks versions error for delete app. %v", err), http.StatusInternalServerError)
		return
	}

	log.Debugf("app %s has %d versions", appId, len(versions))

	// mark app op status
	if err := r.memoAppStatus(appId, types.OpStatusDeleting, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to deleting got error: %v", err), http.StatusInternalServerError)
		return
	}

	go func() {
		var err error

		// defer to mark op status
		defer func() {
			if err != nil {
				log.Errorf("delete app %s error: %v", appId, err)
				r.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("delete app error: %v", err))
			} else {
				log.Printf("delete app %s succeed", appId)
			}
		}()

		log.Printf("Preparing to delete App %s with %d tasks %d versions", appId, len(tasks), len(versions))

		err = r.delApp(appId, tasks, versions)
	}()

	writeJSON(w, http.StatusNoContent, "")
}

func (r *Server) scaleApp(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusLocked)
		return
	}

	var scale types.Scale
	if err := decode(req.Body, &scale); err != nil {
		http.Error(w, fmt.Sprintf("decode scale param error: %v", err), http.StatusBadRequest)
		return
	}

	tasks, err := r.db.ListTasks(appId)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for scale app. %v", err), http.StatusInternalServerError)
		return
	}

	var (
		current = len(tasks)
		goal    = scale.Instances
		ips     = scale.IPs // TODO(nmg): remove after automatic ipam
	)

	if goal < 0 {
		http.Error(w, "the goal count can't be negative", http.StatusBadRequest)
		return
	}

	if goal == current {
		writeJSON(w, http.StatusNotModified, "instances not changed")
		return
	}

	ver, err := r.db.GetVersion(app.ID, app.Version[0])
	if err != nil {
		http.Error(w, fmt.Sprintf("get version got error for scale app. %v", err), http.StatusInternalServerError)
		return
	}

	newVer := ver
	newVer.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	newVer.Instances = int32(goal)
	newVer.IPs = ips

	if err := r.db.CreateVersion(appId, newVer); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	if goal < current { // scale dwon
		if err := r.memoAppStatus(appId, types.OpStatusScalingDown, ""); err != nil {
			http.Error(w, fmt.Sprintf("update app opstatus to scaling down error: %v", err), http.StatusInternalServerError)
			return
		}

		go func() {
			var err error

			defer func() {
				if err != nil {
					log.Errorf("scale down app %s error: %v", appId, err)
					r.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("scale down app error: %v", err))
				} else {
					r.memoAppStatus(appId, types.OpStatusNoop, "")
					log.Printf("scale down app %s succeed", appId)
				}
			}()

			log.Printf("Preparing to scale down App %s", appId)

			types.TaskList(tasks).Reverse() // TODO
			var (
				killing = tasks[:current-goal]
				wg      sync.WaitGroup
				succeed int64
			)
			for _, task := range killing {
				wg.Add(1)

				go func(task *types.Task) {
					defer wg.Done()

					if err := r.delTask(appId, task); err != nil {
						return
					}

					atomic.AddInt64(&succeed, 1)
				}(task)
			}
			wg.Wait()

			if int(succeed) != len(killing) {
				err = fmt.Errorf("%d tasks failed", len(killing)-int(succeed))
			}
		}()

		writeJSON(w, http.StatusAccepted, "accepted")
		return
	}

	// scale up

	version, err := r.db.GetVersion(appId, app.Version[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	net := version.Container.Docker.Network
	if net != "host" && net != "bridge" {
		if len(ips) < int(goal-current) {
			http.Error(w, fmt.Sprintf("IP number cannot be less than the instance number"), http.StatusBadRequest)
			return
		}
	}

	if err := r.memoAppStatus(appId, types.OpStatusScalingUp, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to scaling up error: %v", err), http.StatusInternalServerError)
		return
	}
	go func() {
		var err error

		// defer to mark op status
		defer func() {
			if err != nil {
				log.Errorf("scale up app %s error: %v", appId, err)
				r.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("scale up app error: %v", err))
			} else {
				log.Printf("scale up app %s succeed", appId)
				r.memoAppStatus(appId, types.OpStatusNoop, "")
			}
		}()

		log.Printf("Preparing to scale up App %s", appId)

		var (
			tasks = []*mesos.Task{}
		)

		// prepare for all of runtime tasks & db tasks
		for i := current; i < goal; i++ {
			var (
				name    = fmt.Sprintf("%d.%s", i, appId)
				id      = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
				restart = version.RestartPolicy
				retries = 3
			)

			if restart != nil && restart.Retries >= 0 {
				retries = restart.Retries
			}

			// runtime tasks
			cfg := types.NewTaskConfig(version, i)
			t := mesos.NewTask(cfg, id, name)
			tasks = append(tasks, t)

			// db tasks
			task := &types.Task{
				ID:         id,
				Name:       name,
				Weight:     100,
				Status:     "pending",
				Healthy:    types.TaskHealthyUnset,
				Version:    version.ID,
				MaxRetries: retries,
				Created:    time.Now(),
				Updated:    time.Now(),
			}
			if version.IsHealthSet() {
				task.Healthy = types.TaskUnHealthy
			}

			if err = r.db.CreateTask(appId, task); err != nil {
				err = fmt.Errorf("create db task failed: %v", err)
				return
			}
		}

		err = r.driver.LaunchTasks(tasks)
		if err != nil {
			err = fmt.Errorf("launch tasks got error: %v", err)
			return
		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) updateApp(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusLocked)
		return
	}

	newVer := new(types.Version)
	if err := decode(req.Body, newVer); err != nil {
		http.Error(w, fmt.Sprintf("decode update version got error: %v", err), http.StatusBadRequest)
		return
	}

	if err := newVer.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	newVer.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	if err := r.db.CreateVersion(appId, newVer); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for update app. %v", err), http.StatusInternalServerError)
		return
	}

	if err := r.memoAppStatus(appId, types.OpStatusUpdating, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to rolling-update got error: %v", err), http.StatusInternalServerError)
		return
	}

	var (
		delay     = float64(1)
		onfailure = types.UpdateStop
	)

	policy := newVer.UpdatePolicy
	if policy != nil {
		delay = policy.Delay
		onfailure = policy.OnFailure
	}

	types.TaskList(tasks).Sort()
	pending := tasks

	go func() {
		var err error

		defer func() {
			if err != nil {
				log.Errorf("update app %s error: %v", appId, err)
				r.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("update app error: %v", err))
			} else {
				log.Printf("update app %s succeed", appId)
				r.memoAppStatus(appId, types.OpStatusNoop, "")
			}
		}()

		log.Printf("Preparing to update App %s", appId)

		for i, t := range pending {

			// kill & remove old
			if err = r.delTask(appId, t); err != nil {
				err = fmt.Errorf("remove old task error: %v", err)
				return
			}

			// db save new task
			var (
				name    = t.Name
				id      = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
				restart = newVer.RestartPolicy
				retries = 3
			)

			if restart != nil && restart.Retries >= 0 {
				retries = restart.Retries
			}

			task := &types.Task{
				ID:         id,
				Name:       name,
				Weight:     100,
				Status:     "pending",
				Healthy:    types.TaskHealthyUnset,
				Version:    newVer.ID,
				MaxRetries: retries,
				Created:    t.Created,
				Updated:    time.Now(),
			}
			if newVer.IsHealthSet() {
				task.Healthy = types.TaskUnHealthy
			}

			if err = r.db.CreateTask(appId, task); err != nil {
				err = fmt.Errorf("create new db task error: %v", err)
				return
			}

			// launch runtime new task
			cfg := types.NewTaskConfig(newVer, i)
			m := mesos.NewTask(cfg, task.ID, task.Name)
			tasks := []*mesos.Task{m}

			if err = r.driver.LaunchTasks(tasks); err != nil {
				err = fmt.Errorf("launch new runtime task error: %v", err)

				task.Status = "Failed"
				task.ErrMsg = err.Error()
				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", id, err)
				}

				if onfailure == types.UpdateStop {
					return
				}
			}

			// notify proxy
			time.Sleep(time.Duration(delay) * time.Second)
		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (s *Server) startApp(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := s.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusLocked)
		return
	}

	ver, err := s.db.GetVersion(appId, app.Version[0])
	if err != nil {
		http.Error(w, fmt.Sprintf("get app version error: %v", err), http.StatusInternalServerError)
		return
	}

	if err := s.memoAppStatus(appId, types.OpStatusStarting, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to stopping got error: %v", err), http.StatusInternalServerError)
		return
	}

	var (
		count   = int(ver.Instances)
		restart = ver.RestartPolicy
		retries = 3
	)

	if restart != nil && restart.Retries >= 0 {
		retries = restart.Retries
	}

	go func(appId string) {
		var err error

		// defer to mark op status
		defer func() {
			if err != nil {
				log.Errorf("start app %s error: %v", appId, err)
				s.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("start app error: %v", err))
			} else {
				log.Printf("start app %s succeed", appId)
				s.memoAppStatus(appId, types.OpStatusNoop, "")
			}
		}()

		log.Printf("Preparing to start App %s", appId)

		tasks := []*mesos.Task{}
		for i := 0; i < count; i++ {
			var (
				name = fmt.Sprintf("%d.%s", i, appId)
				id   = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
			)

			// runtime tasks
			cfg := types.NewTaskConfig(ver, i)
			t := mesos.NewTask(cfg, id, name)
			tasks = append(tasks, t)

			// save db tasks
			// TODO move db task creation to each runtime task logic
			task := &types.Task{
				ID:         id,
				Name:       name,
				Weight:     100,
				Status:     "pending",
				Healthy:    types.TaskHealthyUnset,
				Version:    ver.ID,
				MaxRetries: retries,
				Created:    time.Now(),
				Updated:    time.Now(),
			}
			if ver.IsHealthSet() {
				task.Healthy = types.TaskUnHealthy
			}

			log.Debugf("Create task %s in db", task.ID)
			if err = s.db.CreateTask(app.ID, task); err != nil {
				err = fmt.Errorf("create db task failed: %v", err)
				return
			}
		}

		err = s.driver.LaunchTasks(tasks)
		if err != nil {
			err = fmt.Errorf("launch tasks got error: %v", err)
			return
		}
	}(appId)

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (s *Server) stopApp(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := s.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusLocked)
		return
	}

	tasks, err := s.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for stopping. %v", err), http.StatusInternalServerError)
		return
	}

	if err := s.memoAppStatus(appId, types.OpStatusStopping, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to stopping got error: %v", err), http.StatusInternalServerError)
		return
	}

	go func() {
		var err error

		defer func() {
			if err != nil {
				log.Errorf("stop app %s error: %v", appId, err)
				s.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("stop app error: %v", err))
			} else {
				log.Printf("stop app %s succeed", appId)
				s.memoAppStatus(appId, types.OpStatusNoop, "")
			}
		}()

		log.Printf("Preparing to stop App %s", appId)

		var wg sync.WaitGroup
		for _, task := range tasks {
			wg.Add(1)
			go func(task *types.Task) {
				defer wg.Done()

				if err := s.delTask(appId, task); err != nil {
					log.Errorf("app %s stop task %s error: %v", appId, task.ID)
				}
			}(task)
		}

		wg.Wait()
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) canaryUpdate(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s := app.OpStatus; s != types.OpStatusNoop && s != types.OpStatusCanaryUnfinished {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", s), http.StatusLocked)
		return
	}

	// obtain new version & policy & verify
	canary := new(types.CanaryUpdateBody)
	if err := decode(req.Body, canary); err != nil {
		http.Error(w, fmt.Sprintf("decode gray publish body got error: %v", err), http.StatusBadRequest)
		return
	}

	var (
		newVer    = canary.Version
		value     = canary.Value
		count     = canary.Instances
		onfailure = canary.OnFailure
		delay     = canary.Delay
	)

	// obtain new version
	switch s := app.OpStatus; s {

	case types.OpStatusNoop: // from new request version on begin & save
		if newVer == nil {
			http.Error(w, "new version required on canary update begin", http.StatusBadRequest)
			return
		}

		// db save
		// TODO rewrite more fields which can't be changed before db saving
		newVer.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
		if err := r.db.CreateVersion(appId, newVer); err != nil {
			http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
			return
		}

	case types.OpStatusCanaryUnfinished: // from previous saved version
		versions, err := r.db.ListVersions(app.ID)
		if err != nil {
			http.Error(w, fmt.Sprintf("list versions error for canary update. %v", err), http.StatusInternalServerError)
			return
		}
		if len(versions) < 2 {
			http.Error(w, "no new version for canary update", http.StatusBadRequest)
			return
		}
		types.VersionList(versions).Reverse()
		newVer = versions[0]
	}

	if value <= 0 || value > 1 {
		http.Error(w, "canary value must between (0, 1]", http.StatusBadRequest)
		return
	}

	if count == 0 {
		count = 1
	}

	if delay == 0 {
		delay = types.DefaultCanaryUpdateDelay
	}

	// obtain all db tasks
	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for canary update. %v", err), http.StatusInternalServerError)
		return
	}

	types.TaskList(tasks).Sort()

	// current progress
	new := 0
	for _, task := range tasks {
		if task.Version == newVer.ID {
			new++
		}
	}

	var (
		total = len(tasks)
		goal  = new + count
	)

	if goal >= total {
		goal = total
		if value != 1 {
			value = 1
		}
	}

	newWeight := utils.ComputeWeight(float64(goal), float64(total), value)
	pending := tasks[:goal]
	oldTasks := tasks[goal:]

	// mark app db status
	if err := r.memoAppStatus(appId, types.OpStatusCanaryUpdating, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to canary-update got error: %v", err), http.StatusInternalServerError)
		return
	}

	go func() {
		var (
			err      error
			progress int
		)

		// defer to mark app db status
		defer func() {
			var (
				errmsg   string
				opStatus = types.OpStatusCanaryUnfinished
			)

			if err != nil {
				log.Errorf("canary update app %s error: %v", appId, err)
				errmsg = fmt.Sprintf("canary update error: %v", err)
			} else {
				log.Printf("canary update app %s succeed", appId)
			}

			if progress >= total {
				opStatus = types.OpStatusNoop
			}

			r.memoAppStatus(appId, opStatus, errmsg)
		}()

		log.Printf("Preparing to canary update App %s", appId)

		for i, t := range pending {
			progress = i + 1

			if t.Version == newVer.ID {
				t.Weight = newWeight

				if uerr := r.db.UpdateTask(appId, t); uerr != nil {
					err = fmt.Errorf("update task %s weight got error: %v", t.ID, uerr)
					return
				}
				// TODO notify proxy

				log.Debugf("Sending task event to proxy for weight changed. taskId: %s weight: %.f", t.ID, newWeight)
				if err := r.driver.SendEvent(appId, t); err != nil {
					log.Errorf("Sending event got error: %v", err)
				}

				continue
			}

			// remove old task
			if err = r.delTask(appId, t); err != nil {
				err = fmt.Errorf("remove old task %s error: %v", t.ID, err)
				return
			}

			var (
				name    = t.Name
				id      = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
				restart = newVer.RestartPolicy
				retries = 3
			)

			if restart != nil && restart.Retries >= 0 {
				retries = restart.Retries
			}

			// db save new task
			task := &types.Task{
				ID:         id,
				Name:       name,
				Weight:     newWeight,
				Healthy:    types.TaskHealthyUnset,
				Version:    newVer.ID,
				MaxRetries: retries,
				Created:    t.Created,
				Updated:    time.Now(),
			}
			if newVer.IsHealthSet() {
				task.Healthy = types.TaskUnHealthy
			}

			if err = r.db.CreateTask(appId, task); err != nil {
				err = fmt.Errorf("create db task %s error: %v", id, err)
				return
			}

			// launch new runtime task
			cfg := types.NewTaskConfig(newVer, i+new)
			m := mesos.NewTask(cfg, task.ID, task.Name)
			tasks := []*mesos.Task{m}

			if err = r.driver.LaunchTasks(tasks); err != nil {
				task.Status = "Failed"
				task.ErrMsg = err.Error()
				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", id, err)
				}

				if onfailure == types.CanaryUpdateOnFailureStop {
					return
				}
			}

			time.Sleep(time.Duration(delay) * time.Second)
		}

		// reset the rest of task's weight to 100.
		for _, task := range oldTasks {
			if task.Weight == 0 {
				task.Weight = 100
			}

			log.Debugf("updating weight to 100 for task %s", task.ID)
			if uerr := r.db.UpdateTask(appId, task); uerr != nil {
				err = fmt.Errorf("update task %s weight got error: %v", task.ID, uerr)
				return
			}

			// notify proxy
			log.Debugf("Sending task event to proxy for weight changed. taskId: %s weight: 100", task.ID)
			if err := r.driver.SendEvent(appId, task); err != nil {
				log.Errorf("updateWeights(): sending task %s event failed: %v", task.ID, err)
			}

		}

	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) rollback(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusLocked)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for rollback app. %v", err), http.StatusInternalServerError)
		return
	}

	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list versions got error for rollback app. %v", err), http.StatusInternalServerError)
		return
	}

	verId := req.Form.Get("version")

	var desired *types.Version

	if verId != "" {
		ver, err := r.db.GetVersion(appId, verId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		desired = ver
	}

	if verId == "" {
		if len(versions) < 2 {
			http.Error(w, fmt.Sprintf("no more versions to rollback"), http.StatusInternalServerError)
			return
		}

		types.VersionList(versions).Sort()
		for idx, ver := range versions {
			if ver.ID == app.Version[0] {
				if (idx - 1) < 0 {
					http.Error(w, fmt.Sprintf("version error"), http.StatusInternalServerError)
					return
				}
				desired = versions[idx-1]
			}
		}
	}

	if err := r.memoAppStatus(appId, types.OpStatusRollback, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to rolling-back got error: %v", err), http.StatusInternalServerError)
		return
	}

	// TODO
	types.TaskList(tasks).Reverse()

	go func() {
		var err error

		defer func() {
			if err != nil {
				log.Errorf("rollback app %s error: %v", appId, err)
				r.memoAppStatus(appId, types.OpStatusNoop, fmt.Sprintf("rollback app error: %v", err))
			} else {
				log.Printf("rollback app %s succeed", appId)
				r.memoAppStatus(appId, types.OpStatusNoop, "")
			}
		}()

		log.Printf("Preparing to rollback App %s", appId)

		for i, t := range tasks {

			// remove old task
			if err = r.delTask(appId, t); err != nil {
				err = fmt.Errorf("remove old task error: %v", err)
				return
			}

			// save db task
			var (
				name    = t.Name
				id      = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
				restart = desired.RestartPolicy
				retries = 3
			)

			if restart != nil && restart.Retries >= 0 {
				retries = restart.Retries
			}

			task := &types.Task{
				ID:         id,
				Name:       name,
				Weight:     100,
				Status:     "updating",
				Version:    desired.ID,
				Healthy:    types.TaskHealthyUnset,
				MaxRetries: retries,
				Created:    t.Created,
				Updated:    time.Now(),
			}

			if desired.IsHealthSet() {
				task.Healthy = types.TaskUnHealthy
			}

			if err = r.db.CreateTask(appId, task); err != nil {
				err = fmt.Errorf("create db task error: %v", err)
				return
			}

			// launch runtime task
			cfg := types.NewTaskConfig(desired, i)
			m := mesos.NewTask(cfg, task.ID, task.Name)
			tasks := []*mesos.Task{m}

			if err = r.driver.LaunchTasks(tasks); err != nil {
				err = fmt.Errorf("launch runtime task %s error: %v", task.ID, err)

				task.Status = "Failed"
				task.ErrMsg = fmt.Sprintf("launch task failed: %v", err)
				if err = r.db.UpdateTask(appId, task); err != nil {
					log.Errorf("update task %s got error: %v", task.ID, err)
				}
			}
		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) updateWeights(w http.ResponseWriter, req *http.Request) {
	var (
		appId = mux.Vars(req)["app_id"]
	)

	// check for operation permission.

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s := app.OpStatus; s != types.OpStatusCanaryUnfinished {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", s), http.StatusLocked)
		return
	}

	var body types.UpdateWeightsBody
	if err := decode(req.Body, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	value := body.Value
	if value <= 0 || value > 1 {
		http.Error(w, "Invalid value. value must be between 0 and 1.(0 < value <= 1)", http.StatusBadRequest)
		return
	}

	// check if the app has new version.
	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list versions got error for rollback app. %v", err), http.StatusInternalServerError)
		return
	}

	// app has no new version
	if len(versions) < 2 {
		http.Error(w, fmt.Sprintf("no more versions to update"), http.StatusInternalServerError)
		return
	}

	types.VersionList(versions).Reverse()

	var (
		newVer = versions[0]
	)

	// get current new tasks

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for update weights. %v", err), http.StatusInternalServerError)
		return
	}
	types.TaskList(tasks).Sort()

	// current new tasks
	new := 0
	for _, task := range tasks {
		if task.Version == newVer.ID {
			new++
		}
	}

	if new == 0 {
		http.Error(w, "no more new task to update weight", http.StatusInternalServerError)
		return
	}

	var (
		total     = len(tasks)
		pending   = tasks[:new]
		newWeight = utils.ComputeWeight(float64(new), float64(total), value)
	)

	// mark app db status
	if err := r.memoAppStatus(appId, types.OpStatusWeightUpdating, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to weight-updating got error: %v", err), http.StatusInternalServerError)
		return
	}

	go func() {
		var (
			errmsg   string
			opStatus = types.OpStatusCanaryUnfinished
		)

		defer func() {
			if errmsg != "" {
				log.Errorf("weight-updating app %s error: %v", appId, err)
			} else {
				log.Errorf("weight-updating app %s succeed", appId)
			}
			r.memoAppStatus(appId, opStatus, errmsg)
		}()

		log.Printf("Preparing to weight-updating App %s", appId)

		for _, task := range pending {
			task.Weight = newWeight

			log.Debugf("updating weight to %f for task %s", newWeight, task.ID)
			if err := r.db.UpdateTask(appId, task); err != nil {
				errmsg = fmt.Sprintf("update task %s weight got error: %v", task.ID, err)
				log.Error(errmsg)
				return
			}

			// notify proxy
			log.Debugf("Sending task event to proxy for weight changed. taskId: %s weight: %.f", task.ID, newWeight)
			if err := r.driver.SendEvent(appId, task); err != nil {
				log.Errorf("updateWeights(): sending task %s event failed: %v", task.ID, err)
			}
		}

		// set the old task's weight to 0 if the new tasks want 100% traffics.
		if value == 1 {
			oldTasks := tasks[new:]
			for _, task := range oldTasks {
				task.Weight = 0

				log.Debugf("updating weight to 0 for task %s", task.ID)
				if err := r.db.UpdateTask(appId, task); err != nil {
					errmsg = fmt.Sprintf("update task %s weight got error: %v", task.ID, err)
					log.Error(errmsg)
					return
				}

				// notify proxy
				log.Debugf("Sending task event to proxy for weight changed. taskId: %s weight: 0", task.ID)
				if err := r.driver.SendEvent(appId, task); err != nil {
					log.Errorf("updateWeights(): sending task %s event failed: %v", task.ID, err)
				}

			}
		}
	}()

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) getTasks(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	tasks, err := r.db.ListTasks(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, tasks)
}

func (r *Server) getTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	task, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, task)
}

func (r *Server) updateWeight(w http.ResponseWriter, req *http.Request) {
	var body types.UpdateWeightBody
	if err := decode(req.Body, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	task, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	task.Weight = body.Weight

	if err := r.db.UpdateTask(appId, task); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// notify proxy

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) getVersions(w http.ResponseWriter, req *http.Request) {
	appId := mux.Vars(req)["app_id"]

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list versions got error for get versions. %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, versions)
}

func (r *Server) getVersion(w http.ResponseWriter, req *http.Request) {
	var (
		vars  = mux.Vars(req)
		appId = vars["app_id"]
		verId = vars["version_id"]
	)

	version, err := r.db.GetVersion(appId, verId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, version)
}

// TODO(nmg): named version should be supported.
func (r *Server) createVersion(w http.ResponseWriter, req *http.Request) {
	var (
		vars  = mux.Vars(req)
		appId = vars["app_id"]
	)

	if err := checkForJSON(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var version types.Version
	if err := decode(req.Body, &version); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := version.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	version.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())

	if err := r.db.CreateVersion(appId, &version); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"id": version.ID})
}

func filterByLabelsSelectors(labelsSelector labels.Selector, appLabels map[string]string) bool {
	if labelsSelector == nil {
		return true
	}

	return labelsSelector.Matches(labels.Set(appLabels))
}

func filterByFieldsSelectors(fieldSelector fields.Selector, ver *types.Version) bool {
	if fieldSelector == nil {
		return true
	}

	// TODO(upccup): there maybe exist better way to got a field/value map
	fieldMap := make(map[string]string)
	fieldMap["runAs"] = ver.RunAs
	return fieldSelector.Matches(fields.Set(fieldMap))
}

func (r *Server) deleteTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	task, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.delTask(appId, task); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusNoContent, "")
}

func (r *Server) deleteTasks(w http.ResponseWriter, req *http.Request) {
	var (
		vars  = mux.Vars(req)
		appId = vars["app_id"]
	)

	app, err := r.db.GetApp(appId)
	if err != nil {
		if r.db.IsErrNotFound(err) {
			http.Error(w, fmt.Sprintf("app %s not exists", appId), http.StatusNotFound)
			return
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tasks, err := r.db.ListTasks(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list tasks got error for delete tasks. %v", err), http.StatusInternalServerError)
		return
	}

	for _, task := range tasks {
		go func(task *types.Task, appId string) {
			if err := r.delTask(appId, task); err != nil {
				return
			}

		}(task, app.ID)
	}

	writeJSON(w, http.StatusNoContent, "")
}

func (r *Server) updateTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	var version types.Version
	if err := decode(req.Body, &version); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := version.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	version.ID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	if err := r.db.CreateVersion(appId, &version); err != nil {
		http.Error(w, fmt.Sprintf("create app version failed: %v", err), http.StatusInternalServerError)
		return
	}

	t, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.delTask(appId, t); err != nil {
		return
	}

	seq := strings.SplitN(t.Name, ".", 2)[0]
	idx, _ := strconv.Atoi(seq)
	cfg := types.NewTaskConfig(&version, idx)

	var (
		name    = t.Name
		id      = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
		restart = version.RestartPolicy
		retries = 3
	)

	if restart != nil && restart.Retries >= 0 {
		retries = restart.Retries
	}

	task := &types.Task{
		ID:         id,
		Name:       name,
		Weight:     100,
		Status:     "updating",
		Version:    version.ID,
		MaxRetries: retries,
		Created:    t.Created,
		Updated:    time.Now(),
	}

	if err := r.db.CreateTask(appId, task); err != nil {
		log.Errorf("create task failed: %s", err)
		return
	}

	m := mesos.NewTask(cfg, task.ID, task.Name)
	tasks := []*mesos.Task{m}

	if err := r.driver.LaunchTasks(tasks); err != nil {
		log.Errorf("launch task %s got error: %v", task.ID, err)

		task.Status = "Failed"
		task.ErrMsg = fmt.Sprintf("launch task failed: %v", err)

		if err = r.db.UpdateTask(appId, task); err != nil {
			log.Errorf("update task %s got error: %v", t.ID, err)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusAccepted, "accepted")
}

func (r *Server) rollbackTask(w http.ResponseWriter, req *http.Request) {
	var (
		vars   = mux.Vars(req)
		appId  = vars["app_id"]
		taskId = vars["task_id"]
	)

	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	app, err := r.db.GetApp(appId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.OpStatus != types.OpStatusNoop {
		http.Error(w, fmt.Sprintf("app status is %s, operation not allowed.", app.OpStatus), http.StatusLocked)
		return
	}

	versions, err := r.db.ListVersions(app.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("list versions got error for rollback task. %v", err), http.StatusInternalServerError)
		return
	}

	if err := r.memoAppStatus(appId, types.OpStatusRollback, ""); err != nil {
		http.Error(w, fmt.Sprintf("update app opstatus to rolling-back got error: %v", err), http.StatusInternalServerError)
		return
	}

	defer func() { // TODO format as above
		r.memoAppStatus(appId, types.OpStatusNoop, "")
	}()

	verId := req.Form.Get("version")

	var desired *types.Version

	if verId != "" {
		ver, err := r.db.GetVersion(appId, verId)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		desired = ver
	}

	if verId == "" {
		if len(versions) < 2 {
			http.Error(w, fmt.Sprintf("no more versions to rollback"), http.StatusInternalServerError)
			return
		}

		// TODO
		types.VersionList(versions).Sort()
		for idx, ver := range versions {
			if ver.ID == app.Version[0] {
				if (idx - 1) < 0 {
					http.Error(w, fmt.Sprintf("version error"), http.StatusInternalServerError)
					return
				}
				desired = versions[idx-1]
			}
		}
	}

	t, err := r.db.GetTask(appId, taskId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := r.delTask(appId, t); err != nil {
		return
	}

	seq := strings.SplitN(t.Name, ".", 2)[0]
	idx, _ := strconv.Atoi(seq)
	cfg := types.NewTaskConfig(desired, idx)

	var (
		name    = t.Name
		id      = fmt.Sprintf("%s.%s", utils.RandomString(12), name)
		restart = desired.RestartPolicy
		retries = 3
	)

	if restart != nil && restart.Retries >= 0 {
		retries = restart.Retries
	}

	task := &types.Task{
		ID:         id,
		Name:       name,
		Weight:     100,
		Status:     "updating",
		Version:    desired.ID,
		MaxRetries: retries,
		Created:    t.Created,
		Updated:    time.Now(),
	}

	if err := r.db.CreateTask(appId, task); err != nil {
		log.Errorf("create task failed: %s", err)
		return
	}

	m := mesos.NewTask(cfg, task.ID, task.Name)

	tasks := []*mesos.Task{m}

	if err := r.driver.LaunchTasks(tasks); err != nil {
		log.Errorf("launch task %s got error: %v", task.ID, err)

		task.Status = "Failed"
		task.ErrMsg = fmt.Sprintf("launch task failed: %v", err)

		if err = r.db.UpdateTask(appId, task); err != nil {
			log.Errorf("update task %s got error: %v", t.ID, err)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusAccepted, "accepted")
}

// delApp actually remove App related runtime tasks & db objects
func (r *Server) delApp(appId string, tasks []*types.Task, versions []*types.Version) error {
	var (
		count   = len(tasks)
		succeed = int64(0)
		wg      sync.WaitGroup
	)

	// remove runtime tasks & db tasks firstly
	for _, task := range tasks {
		wg.Add(1)
		go func(appId string, task *types.Task) {
			defer wg.Done()

			if err := r.driver.SendEvent(appId, task); err != nil {
				log.Errorf("Sending event got error: %v", err)
			}

			if err := r.delTask(appId, task); err != nil {
				return
			}

			atomic.AddInt64(&succeed, 1)
		}(appId, task)
	}
	wg.Wait()

	if int(succeed) != count {
		return fmt.Errorf("%d tasks kill / removed failed", count-int(succeed))
	}

	// remove db versions
	for _, version := range versions {
		if err := r.db.DeleteVersion(appId, version.ID); err != nil {
			return fmt.Errorf("Delete version %s for app %s got error: %v", version.ID, appId, err)
		}
	}

	// remove db app
	if err := r.db.DeleteApp(appId); err != nil {
		return fmt.Errorf("Delete app %s got error: %v", appId, err)
	}

	return nil
}

// delTask actually kill runtime task & remove db objects
func (s *Server) delTask(appId string, task *types.Task) error {
	var gracePeriod int64
	if ver, err := s.db.GetVersion(appId, task.Version); err == nil {
		if ver.KillPolicy != nil {
			gracePeriod = ver.KillPolicy.Duration
		}
	}

	if err := s.driver.KillTask(task.ID, task.AgentId, gracePeriod); err != nil {
		log.Errorf("Kill task %s got error: %v", task.ID, err)

		task.OpStatus = fmt.Sprintf("kill task error: %v", err)
		if err = s.db.UpdateTask(appId, task); err != nil {
			log.Errorf("update task %s got error: %v", task.Name, err)
		}

		return err
	}

	if err := s.db.DeleteTask(task.ID); err != nil {
		log.Errorf("Delete task %s got error: %v", task.ID, err)

		task.OpStatus = fmt.Sprintf("delete task error: %v", err)
		if err = s.db.UpdateTask(appId, task); err != nil {
			log.Errorf("update task %s got error: %v", task.Name, err)
		}

		return err
	}

	return nil
}

// short hands to memo update App.OpStatus & App.ErrMsg
// it's the caller responsibility to process the db error.
func (r *Server) memoAppStatus(appId, op, errmsg string) error {
	app, err := r.db.GetApp(appId)
	if err != nil {
		log.Errorf("memoAppStatus() get db app %s error: %v", appId, err)
		return err
	}

	var (
		prevOp = app.OpStatus
	)

	app.OpStatus = op
	app.ErrMsg = errmsg
	app.UpdatedAt = time.Now()

	if err := r.db.UpdateApp(app); err != nil {
		log.Errorf("memoAppStatus() update app db status from %s -> %s error: %v", prevOp, op, err)
		return err
	}

	return nil
}

func (r *Server) checkPortListening(p *types.Proxy) error {
	if p == nil {
		return nil
	}

	if !p.Enabled {
		return nil
	}

	if p.Proxies == nil || len(p.Proxies) == 0 {
		return nil
	}

	for _, proxy := range p.Proxies {
		l, _ := strconv.Atoi(strings.TrimPrefix(proxy.Listen, ":"))

		for _, v := range r.getAgentsListenings() {
			if int64(l) == v {
				return fmt.Errorf("proxy.Listen %d occupied on nodes", l)
			}
		}
	}

	return nil
}

func (r *Server) checkProxyDuplication(p *types.Proxy) error {
	if p == nil {
		return nil
	}

	if !p.Enabled {
		return nil
	}

	proxies, err := r.dbProxies()
	if err != nil {
		return err
	}

	for _, proxy := range p.Proxies {
		// check Listen
		if v := proxy.Listen; v != "" {
			for app, ps := range proxies {
				for _, p := range ps {
					for _, pr := range p.Proxies {
						if pr.Listen == v {
							return fmt.Errorf("proxy.Listen %s conflict to app %s", v, app)
						}
					}
				}
			}
		}

		// check Alias
		if v := proxy.Alias; v != "" {
			for app, ps := range proxies {
				for _, p := range ps {
					for _, pr := range p.Proxies {
						if pr.Alias == v {
							return fmt.Errorf("proxy.Alias %s conflict to app %s", v, app)
						}
					}
				}
			}
		}
	}
	return nil
}

// obtain all of db enabled proxy settings
func (r *Server) dbProxies() (map[string][]*types.Proxy, error) {
	m := make(map[string][]*types.Proxy) // app -> []proxy

	apps, err := r.db.ListApps()
	if err != nil {
		return m, fmt.Errorf("dbProxies() error: %v", err)
	}

	for _, app := range apps {
		m[app.ID] = make([]*types.Proxy, 0, 0)

		for _, vid := range app.Version {
			ver, err := r.db.GetVersion(app.ID, vid)
			if err != nil {
				continue
			}

			if ver.Proxy == nil {
				continue
			}
			if !ver.Proxy.Enabled {
				continue
			}

			m[app.ID] = append(m[app.ID], ver.Proxy)
		}
	}

	return m, nil
}
