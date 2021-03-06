package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/csibroker/csibroker"
	"code.cloudfoundry.org/csibroker/utils"
	"code.cloudfoundry.org/csishim"
	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/goshims/grpcshim"
	"code.cloudfoundry.org/goshims/osshim"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerflags"
	"code.cloudfoundry.org/service-broker-store/brokerstore"
	"github.com/pivotal-cf/brokerapi"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
)

var dataDir = flag.String(
	"dataDir",
	"",
	"[REQUIRED] - Broker's state will be stored here to persist across reboots",
)

var atAddress = flag.String(
	"listenAddr",
	"0.0.0.0:8999",
	"host:port to serve service broker API",
)

var username = flag.String(
	"username",
	"admin",
	"basic auth username to verify on incoming requests",
)

var password = flag.String(
	"password",
	"admin",
	"basic auth password to verify on incoming requests",
)

var serviceSpec = flag.String(
	"serviceSpec",
	"",
	"[REQUIRED] - the file path of the specfile which defines the service",
)

var dbDriver = flag.String(
	"dbDriver",
	"",
	"(optional) database driver name when using SQL to store broker state",
)

var dbHostname = flag.String(
	"dbHostname",
	"",
	"(optional) database hostname when using SQL to store broker state",
)
var dbPort = flag.String(
	"dbPort",
	"",
	"(optional) database port when using SQL to store broker state",
)

var dbName = flag.String(
	"dbName",
	"",
	"(optional) database name when using SQL to store broker state",
)

var dbCACert = flag.String(
	"dbCACert",
	"",
	"(optional) CA Cert to verify SSL connection",
)

var cfServiceName = flag.String(
	"cfServiceName",
	"",
	"(optional) For CF pushed apps, the service name in VCAP_SERVICES where we should find database credentials.  dbDriver must be defined if this option is set, but all other db parameters will be extracted from the service binding.",
)

var (
	dbUsername string
	dbPassword string
)

func main() {
	parseCommandLine()
	parseEnvironment()

	checkParams()

	logger, logSink := newLogger()
	logger.Info("starting")
	defer logger.Info("ends")

	server := createServer(logger)

	if dbgAddr := debugserver.DebugAddress(flag.CommandLine); dbgAddr != "" {
		server = utils.ProcessRunnerFor(grouper.Members{
			{Name: "debug-server", Runner: debugserver.Runner(dbgAddr, logSink)},
			{Name: "broker-api", Runner: server},
		})
	}

	process := ifrit.Invoke(server)
	logger.Info("started")
	utils.UntilTerminated(logger, process)
}

func parseCommandLine() {
	lagerflags.AddFlags(flag.CommandLine)
	debugserver.AddFlags(flag.CommandLine)
	flag.Parse()
}

func checkParams() {
	if *dataDir == "" && *dbDriver == "" {
		fmt.Fprint(os.Stderr, "\nERROR: Either dataDir or db parameters must be provided.\n\n")
		flag.Usage()
		os.Exit(1)
	}

	if *serviceSpec == "" {
		fmt.Fprint(os.Stderr, "\nERROR:serviceSpec must be provided.\n\n")
		flag.Usage()
		os.Exit(1)
	}
}

func newLogger() (lager.Logger, *lager.ReconfigurableSink) {
	lagerConfig := lagerflags.ConfigFromFlags()
	lagerConfig.RedactSecrets = true

	return lagerflags.NewFromConfig("csibroker", lagerConfig)
}

func parseVcapServices(logger lager.Logger, os osshim.Os) {
	if *dbDriver == "" {
		logger.Fatal("missing-db-driver-parameter", errors.New("dbDriver parameter is required for cf deployed broker"))
	}

	// populate db parameters from VCAP_SERVICES and pitch a fit if there isn't one.
	services, hasValue := os.LookupEnv("VCAP_SERVICES")
	if !hasValue {
		logger.Fatal("missing-vcap-services-environment", errors.New("missing VCAP_SERVICES environment"))
	}

	stuff := map[string][]interface{}{}
	err := json.Unmarshal([]byte(services), &stuff)
	if err != nil {
		logger.Fatal("json-unmarshal-error", err)
	}

	stuff2, ok := stuff[*cfServiceName]
	if !ok {
		logger.Fatal("missing-service-binding", errors.New("VCAP_SERVICES missing specified db service"), lager.Data{"stuff": stuff})
	}

	stuff3 := stuff2[0].(map[string]interface{})

	credentials := stuff3["credentials"].(map[string]interface{})
	logger.Debug("credentials-parsed", lager.Data{"credentials": credentials})

	dbUsername = credentials["username"].(string)
	dbPassword = credentials["password"].(string)
	*dbHostname = credentials["hostname"].(string)
	if *dbPort, ok = credentials["port"].(string); !ok {
		*dbPort = fmt.Sprintf("%.0f", credentials["port"].(float64))
	}
	*dbName = credentials["name"].(string)
}

func parseEnvironment() {
	dbUsername, _ = os.LookupEnv("DB_USERNAME")
	dbPassword, _ = os.LookupEnv("DB_PASSWORD")
}

func createServer(logger lager.Logger) ifrit.Runner {
	fileName := filepath.Join(*dataDir, "csi-general-services.json")

	// if we are CF pushed
	if *cfServiceName != "" {
		parseVcapServices(logger, &osshim.OsShim{})
	}

	store := brokerstore.NewStore(logger, *dbDriver, dbUsername, dbPassword, *dbHostname, *dbPort, *dbName, *dbCACert, "", "", "", "", "", fileName, "")
	servicesRegistry, err := csibroker.NewServicesRegistry(
		&csishim.CsiShim{},
		&grpcshim.GrpcShim{},
		*serviceSpec,
		logger,
	)
	if err != nil {
		logger.Error("services-registry-initialize-error", err)
		os.Exit(1)
	}

	serviceBroker, err := csibroker.New(
		logger,
		&osshim.OsShim{},
		clock.NewClock(),
		store,
		servicesRegistry,
	)
	logger.Info("listenAddr: " + *atAddress + ", serviceSpec: " + *serviceSpec)

	if err != nil {
		logger.Error("csibroker-initialize-error", err)
		os.Exit(1)
	}

	credentials := brokerapi.BrokerCredentials{Username: *username, Password: *password}
	handler := brokerapi.New(serviceBroker, logger.Session("broker-api"), credentials)

	return http_server.New(*atAddress, handler)
}
