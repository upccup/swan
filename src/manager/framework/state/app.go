package state

import (
	"errors"
	"fmt"
	"time"

	"github.com/Dataman-Cloud/swan/src/manager/framework/mesos_connector"
	"github.com/Dataman-Cloud/swan/src/types"

	"github.com/Sirupsen/logrus"
)

type AppMode string

var (
	APP_MODE_FIXED      AppMode = "fixed"
	APP_MODE_REPLICATES AppMode = "replicates"
)

type AppInvalidateCallbackFuncs func(app *App)

const (
	APP_STATE_NORMAL              = "normal"
	APP_STATE_MARK_FOR_CREATING   = "creating"
	APP_STATE_MARK_FOR_DELETION   = "deleting"
	APP_STATE_MARK_FOR_UPDATING   = "updating"
	APP_STATE_MARK_FOR_SCALE_UP   = "scale_up"
	APP_STATE_MARK_FOR_SCALE_DOWN = "scale_down"
)

type App struct {
	// app name
	AppId    string           `json:"appId"`
	Versions []*types.Version `json:"versions"`
	Slots    map[int]*Slot    `json:"slots"`

	// app run with CurrentVersion config
	CurrentVersion *types.Version `json:"current_version"`
	// use when app updated, ProposedVersion can either be commit or revert
	ProposedVersion *types.Version `json:"proposed_version"`

	Mode AppMode `json:"mode"` // fixed or repliactes

	OfferAllocatorRef *OfferAllocator
	Created           time.Time
	Updated           time.Time

	State               string
	InvalidateCallbacks map[string][]AppInvalidateCallbackFuncs

	MesosConnector *mesos_connector.MesosConnector
}

func NewApp(version *types.Version, allocator *OfferAllocator, MesosConnector *mesos_connector.MesosConnector) (*App, error) {
	app := &App{
		Versions:          []*types.Version{version},
		Slots:             make(map[int]*Slot),
		CurrentVersion:    version,
		OfferAllocatorRef: allocator,
		AppId:             version.AppId,
		MesosConnector:    MesosConnector,

		Created: time.Now(),
		Updated: time.Now(),

		InvalidateCallbacks: make(map[string][]AppInvalidateCallbackFuncs),
	}

	if version.Mode == "fixed" {
		app.Mode = APP_MODE_FIXED
	} else { // if no mode specified, default should be replicates
		app.Mode = APP_MODE_REPLICATES
	}
	version.ID = fmt.Sprintf("%d", time.Now().Unix())

	for i := 0; i < int(version.Instances); i++ {
		slot := NewSlot(app, version, i)
		app.Slots[i] = slot
	}

	afterAllTasksRunning := func(app *App) {
		if app.RunningInstances() == int(app.CurrentVersion.Instances) {
			logrus.Infof("afterAllTasksRunning func")
			app.SetState(APP_STATE_NORMAL)
		}
	}
	app.SetState(APP_STATE_MARK_FOR_CREATING)
	app.RegisterInvalidateCallbacks(APP_STATE_MARK_FOR_CREATING, afterAllTasksRunning)

	return app, nil
}

// also need user pass ip here
func (app *App) ScaleUp(to int) error {
	if !app.StateIs(APP_STATE_NORMAL) {
		return errors.New("app not in normal state")
	}

	if to <= int(app.CurrentVersion.Instances) {
		return errors.New("scale up expected instances size no less than current size")
	}

	afterScaleUp := func(app *App) {
		if app.RunningInstances() == int(app.CurrentVersion.Instances) {
			logrus.Infof("removeSlot func")
			app.SetState(APP_STATE_NORMAL)
		}
	}
	app.SetState(APP_STATE_MARK_FOR_SCALE_UP)
	app.RegisterInvalidateCallbacks(APP_STATE_MARK_FOR_SCALE_UP, afterScaleUp)

	for i := int(app.CurrentVersion.Instances); i < to; i++ {
		app.Slots[i] = NewSlot(app, app.CurrentVersion, i)
	}
	app.CurrentVersion.Instances = int32(to)
	app.Updated = time.Now()

	return nil
}

func (app *App) ScaleDown(to int) error {
	if !app.StateIs(APP_STATE_NORMAL) {
		return errors.New("app not in normal state")
	}

	if to >= int(app.CurrentVersion.Instances) {
		return errors.New("scale down expected instances size no bigger than current size")
	}

	afterScaleDown := func(app *App) {
		if len(app.Slots) == int(app.CurrentVersion.Instances) &&
			app.MarkForDeletionInstances() == 0 {
			logrus.Infof("afterScaleDown func")
			app.SetState(APP_STATE_NORMAL)
		}
	}

	removeSlot := func(app *App) {
		for k, slot := range app.Slots {
			if slot.MarkForDeletion && (slot.StateIs(SLOT_STATE_TASK_KILLED) || slot.StateIs(SLOT_STATE_TASK_FINISHED) || slot.StateIs(SLOT_STATE_TASK_FAILED)) {
				logrus.Infof("removeSlot func")
				// TODO remove slot from OfferAllocator
				delete(app.Slots, k)
				break
			}
		}
	}

	app.SetState(APP_STATE_MARK_FOR_SCALE_DOWN)
	app.RegisterInvalidateCallbacks(APP_STATE_MARK_FOR_SCALE_DOWN, afterScaleDown, removeSlot)

	for i := int(app.CurrentVersion.Instances); i > to; i-- {
		slot := app.Slots[i-1]
		slot.Kill()
	}

	app.CurrentVersion.Instances = int32(to)
	app.Updated = time.Now()

	return nil
}

// delete a application and all related objects: versions, tasks, slots, proxies, dns record
func (app *App) Delete() error {
	removeSlot := func(app *App) {
		for k, slot := range app.Slots {
			if slot.MarkForDeletion && (slot.StateIs(SLOT_STATE_TASK_KILLED) || slot.StateIs(SLOT_STATE_TASK_FINISHED) || slot.StateIs(SLOT_STATE_TASK_FAILED)) {
				// TODO remove slot from OfferAllocator
				logrus.Infof("removeSlot func")
				delete(app.Slots, k)
				break
			}
		}
	}

	app.SetState(APP_STATE_MARK_FOR_DELETION)
	app.RegisterInvalidateCallbacks(APP_STATE_MARK_FOR_DELETION, removeSlot)

	for _, slot := range app.Slots {
		slot.Kill()
	}

	return nil
}

func (app *App) SetState(state string) {
	app.InvalidateCallbacks = make(map[string][]AppInvalidateCallbackFuncs)
	logrus.Infof("now clearing all InvalidateCallbacks")

	app.State = state
	logrus.Infof("app %s now has state %s", app.AppId, app.State)
}

func (app *App) StateIs(state string) bool {
	return app.State == state
}

func (app *App) Update(version *types.Version) error {
	if !app.StateIs(APP_STATE_NORMAL) {
		return errors.New("app not in normal state")
	}

	if app.ProposedVersion != nil {
		return errors.New("previous rolling update in progress")
	}

	err := app.checkProposedVersionValid(version)
	if err != nil {
		return err
	}
	app.ProposedVersion = version
	app.ProposedVersion.ID = fmt.Sprintf("%d", time.Now().Unix())

	afterUpdateDone := func(app *App) {
		if (app.RollingUpdateInstances() == int(app.CurrentVersion.Instances)) &&
			(app.RunningInstances() == int(app.CurrentVersion.Instances)) { // not perfect as when instances number increase, all instances running might be hard to acheive
			app.SetState(APP_STATE_NORMAL)
			app.CurrentVersion = app.ProposedVersion
			app.Versions = append(app.Versions, app.CurrentVersion)
			app.ProposedVersion = nil

			for _, slot := range app.Slots {
				slot.MarkForRollingUpdate = false
			}
		}
	}

	app.SetState(APP_STATE_MARK_FOR_UPDATING)
	app.RegisterInvalidateCallbacks(APP_STATE_MARK_FOR_UPDATING, afterUpdateDone)

	for i := 0; i < 1; i++ { // current we make first slot update
		slot := app.Slots[i]
		slot.Update(app.ProposedVersion)
	}

	return nil
}

func (app *App) ProceedingRollingUpdate(instances int) error {
	if app.ProposedVersion == nil {
		return errors.New("app not in rolling update state")
	}

	if instances < 1 {
		return errors.New("please specify how many instance want proceeding the update")
	}

	if (instances + app.RollingUpdateInstances()) > int(app.CurrentVersion.Instances) {
		return errors.New("update instances count exceed the maximum instances number")
	}

	for i := 0; i < instances; i++ {
		slot := app.Slots[i+app.RollingUpdateInstances()]
		slot.Update(app.ProposedVersion)
	}

	return nil
}

func (app *App) CancelUpdate() error {
	if app.ProposedVersion == nil {
		return errors.New("app not in rolling update state")
	}

	afterRollbackDone := func(app *App) {
		if app.Slots[0].Version == app.CurrentVersion && // until the first slot has updated to CurrentVersion
			app.RunningInstances() == int(app.CurrentVersion.Instances) { // not perfect as when instances number increase, all instances running might be hard to acheive

			app.SetState(APP_STATE_NORMAL)
			app.ProposedVersion = nil

			for _, slot := range app.Slots {
				slot.MarkForRollingUpdate = false
			}
		}
	}

	app.RemoveInvalidateCallbacks(APP_STATE_MARK_FOR_UPDATING) // deregister callbacks for updateDone in app.Update
	app.RegisterInvalidateCallbacks(APP_STATE_MARK_FOR_UPDATING, afterRollbackDone)

	for i := app.RollingUpdateInstances() - 1; i >= 0; i-- {
		slot := app.Slots[i]
		slot.Update(app.CurrentVersion)
	}
	return nil
}

// called when slot has any update
func (app *App) InvalidateSlots() {
	// handle callback
	if len(app.InvalidateCallbacks[app.State]) > 0 {
		for _, cb := range app.InvalidateCallbacks[app.State] {
			logrus.Infof("calling invalidation callback for state %s", app.State)
			cb(app)
		}
	}

	switch app.State {
	case APP_STATE_MARK_FOR_DELETION:
	case APP_STATE_MARK_FOR_UPDATING:
	case APP_STATE_MARK_FOR_CREATING:
	case APP_STATE_MARK_FOR_SCALE_UP:
	case APP_STATE_MARK_FOR_SCALE_DOWN:
	default:
	}
}

func (app *App) RegisterInvalidateCallbacks(state string, callback ...AppInvalidateCallbackFuncs) {
	app.InvalidateCallbacks[state] = append(app.InvalidateCallbacks[state], callback...)
}

func (app *App) RemoveInvalidateCallbacks(state string) {
	app.InvalidateCallbacks[state] = make([]AppInvalidateCallbackFuncs, 0)
}

// make sure proposed version is valid then applied it to field ProposedVersion
func (app *App) checkProposedVersionValid(version *types.Version) error {
	// mode can not changed
	// runAs can not changed
	// app instances should same as current instances
	return nil
}

func (app *App) RunningInstances() int {
	runningInstances := 0
	for _, slot := range app.Slots {
		if slot.StateIs(SLOT_STATE_TASK_RUNNING) {
			runningInstances += 1
		}
	}

	return runningInstances
}

func (app *App) RollingUpdateInstances() int {
	rollingUpdateInstances := 0
	for _, slot := range app.Slots {
		if slot.MarkForRollingUpdate {
			rollingUpdateInstances += 1
		}
	}

	return rollingUpdateInstances
}

func (app *App) MarkForDeletionInstances() int {
	markForDeletionInstances := 0
	for _, slot := range app.Slots {
		if slot.MarkForDeletion {
			markForDeletionInstances += 1
		}
	}

	return markForDeletionInstances
}

func (app *App) RollbackInstances() int {
	return 0
}

func (app *App) CanBeCleanAfterDeletion() bool {
	return app.StateIs(APP_STATE_MARK_FOR_DELETION) && len(app.Slots) == 0
}

// TODO
func (app *App) PersistToStorage() error {
	return nil
}

func (app *App) IsReplicates() bool {
	return app.Mode == APP_MODE_REPLICATES
}

func (app *App) IsFixed() bool {
	return app.Mode == APP_MODE_FIXED
}