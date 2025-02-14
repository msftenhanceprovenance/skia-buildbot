package powercycle

import (
	"context"
	"io/ioutil"
	"sort"
	"time"

	"github.com/flynn/json5"
	"go.skia.org/infra/go/skerr"
)

// DeviceID is a unique identifier for a given machine or attached device.
type DeviceID string

// DeviceIn returns true if the given id is in the slice of DeviceID.
func DeviceIn(id DeviceID, ids []DeviceID) bool {
	for _, other := range ids {
		if other == id {
			return true
		}
	}
	return false
}

func sortIDs(ids []DeviceID) {
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
}

// Controller abstracts a set of devices that can all be controlled together.
type Controller interface {
	// DeviceIDs returns a list of strings that uniquely identify the devices that can be controlled
	// through this group.
	DeviceIDs() []DeviceID

	// PowerCycle turns the device off for a reasonable amount of time (i.e. 10 seconds) and then
	// turns it back on. If delayOverride is larger than zero it overrides the default delay between
	// turning the port off and on again.
	PowerCycle(ctx context.Context, id DeviceID, delayOverride time.Duration) error
}

// controllerName is a human readable name (hopefully a physical label) for a given Controller.
// It is not really used by the code - this type is primarily for self-documentation purposes.
type controllerName string

// config is the overall structure to aggregate configuration options for different device types.
type config struct {
	// MPower aggregates all mPower configurations.
	MPower map[controllerName]*mPowerConfig `json:"mpower"`

	// EdgeSwitch aggregates all EdgeSwitch configurations.
	EdgeSwitch map[controllerName]*EdgeSwitchConfig `json:"edgeswitch"`
}

// multiController allows us to combine multiple Controller implementations into one.
type multiController struct {
	controllerForID map[DeviceID]Controller
}

// add adds a new Controller.
func (a *multiController) add(client Controller) error {
	for _, id := range client.DeviceIDs() {
		if _, ok := a.controllerForID[id]; ok {
			return skerr.Fmt("Device '%s' already exists.", id)
		}
		a.controllerForID[id] = client
	}
	return nil
}

// DeviceIDs implements the Controller interface.
func (a *multiController) DeviceIDs() []DeviceID {
	ret := make([]DeviceID, 0, len(a.controllerForID))
	for id := range a.controllerForID {
		ret = append(ret, id)
	}
	sortIDs(ret)
	return ret
}

// PowerCycle implements the Controller interface.
func (a *multiController) PowerCycle(ctx context.Context, id DeviceID, delayOverride time.Duration) error {
	ctrl, ok := a.controllerForID[id]
	if !ok {
		return skerr.Fmt("Unknown device id: %s", id)
	}
	return ctrl.PowerCycle(ctx, id, delayOverride)
}

// controllerFromConfig creates a Controll from the given config. If connect is
// true, an attempt will be made to connect to the subclients and errors will be
// returned if they are not accessible.
func controllerFromConfig(ctx context.Context, conf config, connect bool) (Controller, error) {
	ret := &multiController{
		controllerForID: map[DeviceID]Controller{},
	}

	// Add the mpower devices.
	for name, c := range conf.MPower {
		mp, err := newMPowerController(ctx, c, connect)
		if err != nil {
			return nil, skerr.Wrapf(err, "initializing %s", name)
		}
		// TODO(kjlubick) add test for duplicate device names.
		if err := ret.add(mp); err != nil {
			return nil, skerr.Wrapf(err, "incorporating %s", name)
		}
	}

	// Add the EdgeSwitch devices.
	for name, c := range conf.EdgeSwitch {
		es, err := newEdgeSwitchController(ctx, c, connect)
		if err != nil {
			return nil, skerr.Wrapf(err, "initializing %s", name)
		}

		if err := ret.add(es); err != nil {
			return nil, skerr.Wrapf(err, "incorporating %s", name)
		}
	}

	return ret, nil
}

// ControllerFromJSON5 parses a JSON5 file and instantiates the defined devices. If connect is true, an
// attempt will be made to connect to the subclients and errors will be returned if they are not
// accessible.
func ControllerFromJSON5(ctx context.Context, path string, connect bool) (Controller, error) {
	conf, err := readConfig(path)
	if err != nil {
		return nil, skerr.Wrap(err)
	}
	return controllerFromConfig(ctx, conf, connect)
}

// ControllerFromJSON5Bytes parses a JSON5 file and instantiates the defined devices. If connect is true, an
// attempt will be made to connect to the subclients and errors will be returned if they are not
// accessible.
func ControllerFromJSON5Bytes(ctx context.Context, configFileBytes []byte, connect bool) (Controller, error) {
	var conf config
	if err := json5.Unmarshal(configFileBytes, &conf); err != nil {
		return nil, skerr.Wrapf(err, "reading JSON5 bytes")
	}
	return controllerFromConfig(ctx, conf, connect)
}

func readConfig(path string) (config, error) {
	conf := config{}
	jsonBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return conf, skerr.Wrapf(err, "reading %s", path)
	}

	if err := json5.Unmarshal(jsonBytes, &conf); err != nil {
		return conf, skerr.Wrapf(err, "reading JSON5 from %s", path)
	}
	return conf, nil
}
