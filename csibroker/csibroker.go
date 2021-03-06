package csibroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"path"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/goshims/osshim"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/service-broker-store/brokerstore"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/jsonpb"
	"github.com/pivotal-cf/brokerapi"
)

const (
	PermissionVolumeMount = brokerapi.RequiredPermission("volume_mount")
	DefaultContainerPath  = "/var/vcap/data"
)

var ErrEmptySpecFile = errors.New("At least one service must be provided in specfile")

type ErrInvalidService struct {
	Index int
}

func (e ErrInvalidService) Error() string {
	return fmt.Sprintf("Invalid service in specfile at index %d", e.Index)
}

type ErrInvalidSpecFile struct {
	err error
}

func (e ErrInvalidSpecFile) Error() string {
	return fmt.Sprintf("Invalid specfile %s", e.err.Error())
}

type ServiceFingerPrint struct {
	Name   string
	Volume *csi.Volume
}

type Service struct {
	DriverName string `json:"driver_name"`
	ConnAddr   string `json:"connection_address"`

	brokerapi.Service
}

type lock interface {
	Lock()
	Unlock()
}

type Broker struct {
	logger           lager.Logger
	os               osshim.Os
	mutex            lock
	clock            clock.Clock
	servicesRegistry ServicesRegistry
	store            brokerstore.Store
	controllerProbed bool
}

func New(
	logger lager.Logger,
	os osshim.Os,
	clock clock.Clock,
	store brokerstore.Store,
	servicesRegistry ServicesRegistry,
) (*Broker, error) {
	logger = logger.Session("new-csi-broker")
	logger.Info("start")
	defer logger.Info("end")

	theBroker := Broker{
		logger:           logger,
		os:               os,
		mutex:            &sync.Mutex{},
		clock:            clock,
		store:            store,
		servicesRegistry: servicesRegistry,
		controllerProbed: false,
	}

	err := store.Restore(logger)

	return &theBroker, err
}

func (b *Broker) Services(_ context.Context) []brokerapi.Service {
	logger := b.logger.Session("services")
	logger.Info("start")
	defer logger.Info("end")

	return b.servicesRegistry.BrokerServices()
}

func (b *Broker) Provision(context context.Context, instanceID string, details brokerapi.ProvisionDetails, asyncAllowed bool) (_ brokerapi.ProvisionedServiceSpec, e error) {
	err := b.probeController(details.ServiceID)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}
	logger := b.logger.Session("provision").WithData(lager.Data{"instanceID": instanceID, "details": details})
	logger.Info("start")
	defer logger.Info("end")

	var configuration csi.CreateVolumeRequest

	logger.Debug("provision-raw-parameters", lager.Data{"RawParameters": details.RawParameters})
	err = jsonpb.UnmarshalString(string(details.RawParameters), &configuration)
	if err != nil {
		logger.Error("provision-raw-parameters-decode-error", err)
		return brokerapi.ProvisionedServiceSpec{}, brokerapi.ErrRawParamsInvalid
	}
	if configuration.Name == "" {
		return brokerapi.ProvisionedServiceSpec{}, errors.New("config requires a \"name\"")
	}

	if len(configuration.GetVolumeCapabilities()) == 0 {
		return brokerapi.ProvisionedServiceSpec{}, errors.New("config requires \"volume_capabilities\"")
	}

	controllerClient, err := b.servicesRegistry.ControllerClient(details.ServiceID)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}
	response, err := controllerClient.CreateVolume(context, &configuration)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}

	volInfo := response.GetVolume()

	b.mutex.Lock()
	defer b.mutex.Unlock()
	defer func() {
		out := b.store.Save(logger)
		if e == nil {
			e = out
		}
	}()

	fingerprint := ServiceFingerPrint{
		configuration.Name,
		volInfo,
	}
	instanceDetails := brokerstore.ServiceInstance{
		details.ServiceID,
		details.PlanID,
		details.OrganizationGUID,
		details.SpaceGUID,
		fingerprint,
	}

	if b.instanceConflicts(instanceDetails, instanceID) {
		return brokerapi.ProvisionedServiceSpec{}, brokerapi.ErrInstanceAlreadyExists
	}
	err = b.store.CreateInstanceDetails(instanceID, instanceDetails)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("failed to store instance details %s", instanceID)
	}
	logger.Info("service-instance-created", lager.Data{"instanceDetails": instanceDetails})

	return brokerapi.ProvisionedServiceSpec{IsAsync: false}, nil
}

func (b *Broker) Deprovision(context context.Context, instanceID string, details brokerapi.DeprovisionDetails, asyncAllowed bool) (_ brokerapi.DeprovisionServiceSpec, e error) {
	err := b.probeController(details.ServiceID)
	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, err
	}
	logger := b.logger.Session("deprovision")
	logger.Info("start")
	defer logger.Info("end")

	var configuration csi.DeleteVolumeRequest

	if instanceID == "" {
		return brokerapi.DeprovisionServiceSpec{}, errors.New("volume deletion requires instance ID")
	}
	if details.PlanID == "" {
		return brokerapi.DeprovisionServiceSpec{}, errors.New("volume deletion requires \"plan_id\"")
	}
	if details.ServiceID == "" {
		return brokerapi.DeprovisionServiceSpec{}, errors.New("volume deletion requires \"service_id\"")
	}

	instanceDetails, err := b.store.RetrieveInstanceDetails(instanceID)
	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, brokerapi.ErrInstanceDoesNotExist
	}

	configuration.Secrets = map[string]string{}

	fingerprint, err := getFingerprint(instanceDetails.ServiceFingerPrint)

	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, err
	}

	configuration.VolumeId = fingerprint.Volume.VolumeId

	controllerClient, err := b.servicesRegistry.ControllerClient(details.ServiceID)
	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, err
	}

	_, err = controllerClient.DeleteVolume(context, &configuration)
	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, err
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()
	defer func() {
		out := b.store.Save(logger)
		if e == nil {
			e = out
		}
	}()

	err = b.store.DeleteInstanceDetails(instanceID)
	if err != nil {
		return brokerapi.DeprovisionServiceSpec{}, err
	}

	return brokerapi.DeprovisionServiceSpec{IsAsync: false, OperationData: "deprovision"}, nil
}

func (b *Broker) Bind(context context.Context, instanceID string, bindingID string, bindDetails brokerapi.BindDetails) (_ brokerapi.Binding, e error) {
	err := b.probeController(bindDetails.ServiceID)
	if err != nil {
		return brokerapi.Binding{}, err
	}
	logger := b.logger.Session("bind")
	logger.Info("start", lager.Data{"bindingID": bindingID, "details": bindDetails})
	defer logger.Info("end")

	b.mutex.Lock()
	defer b.mutex.Unlock()
	defer func() {
		out := b.store.Save(logger)
		if e == nil {
			e = out
		}
	}()

	logger.Info("starting-csibroker-bind")
	instanceDetails, err := b.store.RetrieveInstanceDetails(instanceID)
	if err != nil {
		return brokerapi.Binding{}, brokerapi.ErrInstanceDoesNotExist
	}

	if bindDetails.AppGUID == "" {
		return brokerapi.Binding{}, brokerapi.ErrAppGuidNotProvided
	}

	fingerprint, err := getFingerprint(instanceDetails.ServiceFingerPrint)

	if err != nil {
		return brokerapi.Binding{}, err
	}

	csiVolumeId := fingerprint.Volume.VolumeId
	csiVolumeAttributes := fingerprint.Volume.VolumeContext

	params := make(map[string]interface{})

	logger.Debug(fmt.Sprintf("bindDetails: %#v", bindDetails.RawParameters))

	if bindDetails.RawParameters != nil {
		err = json.Unmarshal(bindDetails.RawParameters, &params)

		if err != nil {
			return brokerapi.Binding{}, err
		}
	}
	mode, err := evaluateMode(params)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	if b.bindingConflicts(bindingID, bindDetails) {
		return brokerapi.Binding{}, brokerapi.ErrBindingAlreadyExists
	}

	logger.Info("retrieved-instance-details", lager.Data{"instanceDetails": instanceDetails})

	err = b.store.CreateBindingDetails(bindingID, bindDetails)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	volumeId := fmt.Sprintf("%s-volume", instanceID)

	driverName, err := b.servicesRegistry.DriverName(bindDetails.ServiceID)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	logger.Info(fmt.Sprintf("csiVolumeAttributes: %#v", csiVolumeAttributes))

	ret := brokerapi.Binding{
		Credentials: struct{}{}, // if nil, cloud controller chokes on response
		VolumeMounts: []brokerapi.VolumeMount{{
			ContainerDir: evaluateContainerPath(params, instanceID),
			Mode:         mode,
			Driver:       driverName,
			DeviceType:   "shared",
			Device: brokerapi.SharedDevice{
				VolumeId: volumeId,
				MountConfig: map[string]interface{}{
					"id":             csiVolumeId,
					"attributes":     csiVolumeAttributes,
					"binding-params": evaluateId(params),
				},
			},
		}},
	}
	return ret, nil
}

func (b *Broker) Unbind(context context.Context, instanceID string, bindingID string, details brokerapi.UnbindDetails) (e error) {
	err := b.probeController(details.ServiceID)
	if err != nil {
		return err
	}
	logger := b.logger.Session("unbind")
	logger.Info("start")
	defer logger.Info("end")

	b.mutex.Lock()
	defer b.mutex.Unlock()
	defer func() {
		out := b.store.Save(logger)
		if e == nil {
			e = out
		}
	}()

	if _, err := b.store.RetrieveInstanceDetails(instanceID); err != nil {
		return brokerapi.ErrInstanceDoesNotExist
	}

	if _, err := b.store.RetrieveBindingDetails(bindingID); err != nil {
		return brokerapi.ErrBindingDoesNotExist
	}

	if err := b.store.DeleteBindingDetails(bindingID); err != nil {
		return err
	}
	return nil
}

func (b *Broker) Update(context context.Context, instanceID string, details brokerapi.UpdateDetails, asyncAllowed bool) (brokerapi.UpdateServiceSpec, error) {
	panic("not implemented")
}

func (b *Broker) LastOperation(_ context.Context, instanceID string, operationData string) (brokerapi.LastOperation, error) {
	return brokerapi.LastOperation{}, nil
}

func (b *Broker) instanceConflicts(details brokerstore.ServiceInstance, instanceID string) bool {
	return b.store.IsInstanceConflict(instanceID, brokerstore.ServiceInstance(details))
}

func (b *Broker) bindingConflicts(bindingID string, details brokerapi.BindDetails) bool {
	return b.store.IsBindingConflict(bindingID, details)
}

func (b *Broker) probeController(serviceID string) error {
	if !b.controllerProbed {
		identityClient, err := b.servicesRegistry.IdentityClient(serviceID)
		if err != nil {
			return err
		}
		_, err = identityClient.Probe(context.TODO(), &csi.ProbeRequest{})
		if err != nil {
			return err
		}
		b.controllerProbed = true
	}
	return nil
}

func evaluateContainerPath(parameters map[string]interface{}, volId string) string {
	if containerPath, ok := parameters["mount"]; ok && containerPath != "" {
		return containerPath.(string)
	}

	return path.Join(DefaultContainerPath, volId)
}

func evaluateId(parameters map[string]interface{}) map[string]string {
	if _, ok := parameters["uid"]; !ok {
		return nil
	}
	if _, ok := parameters["gid"]; !ok {
		return nil
	}
	return map[string]string{
		"uid": parameters["uid"].(string),
		"gid": parameters["gid"].(string),
	}
}

func evaluateMode(parameters map[string]interface{}) (string, error) {

	if ro, ok := parameters["readonly"]; ok {
		switch ro := ro.(type) {
		case bool:
			return readOnlyToMode(ro), nil
		default:
			return "", brokerapi.ErrRawParamsInvalid
		}
	}
	return "rw", nil
}

func readOnlyToMode(ro bool) string {
	if ro {
		return "r"
	}
	return "rw"
}

func getFingerprint(rawObject interface{}) (*ServiceFingerPrint, error) {
	fingerprint, ok := rawObject.(*ServiceFingerPrint)
	if ok {
		return fingerprint, nil
	}

	// casting didn't work--try marshalling and unmarshalling as the correct type
	rawJson, err := json.Marshal(rawObject)
	if err != nil {
		return nil, err
	}

	fingerprint = &ServiceFingerPrint{}
	err = json.Unmarshal(rawJson, fingerprint)
	if err != nil {
		return nil, err
	}

	return fingerprint, nil
}
