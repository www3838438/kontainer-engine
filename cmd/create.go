package cmd

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"

	"path/filepath"

	"io/ioutil"
	"strconv"

	"fmt"

	"github.com/rancher/kontainer-engine/cluster"
	rpcDriver "github.com/rancher/kontainer-engine/driver"
	"github.com/rancher/kontainer-engine/utils"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const (
	caPem             = "ca.pem"
	clientKey         = "key.pem"
	clientCert        = "cert.pem"
	defaultConfigName = "config.json"
)

// CreateCommand defines the create command
func CreateCommand() cli.Command {
	return cli.Command{
		Name:            "create",
		Usage:           "Create a kubernetes cluster",
		Action:          createWapper,
		SkipFlagParsing: true,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "driver",
				Usage: "Driver to create kubernetes clusters",
			},
		},
	}
}

func createWapper(ctx *cli.Context) error {
	debug := lookUpDebugFlag()
	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	driverName := flagHackLookup("--driver")
	if driverName == "" {
		persistStore := cliPersistStore{}
		// ingore the error as we only care if cluster.name is present
		cls, _ := persistStore.Get(os.Args[len(os.Args)-1])
		if cls.DriverName != "" {
			driverName = cls.DriverName
		} else {
			logrus.Error("Driver name is required")
			return cli.ShowCommandHelp(ctx, "create")
		}
	}
	rpcClient, addr, err := runRPCDriver(driverName)
	if err != nil {
		return err
	}
	driverFlags, err := rpcClient.GetDriverCreateOptions()
	if err != nil {
		return err
	}
	flags := getDriverFlags(driverFlags)
	for i, command := range ctx.App.Commands {
		if command.Name == "create" {
			createCmd := &ctx.App.Commands[i]
			createCmd.SkipFlagParsing = false
			createCmd.Flags = append(createCmd.Flags, flags...)
			createCmd.Action = create
		}
	}
	// append plugin addr if it is built-in driver
	if len(os.Args) > 1 && addr != "" {
		args := []string{os.Args[0], "--plugin-listen-addr", addr}
		args = append(args, os.Args[1:len(os.Args)]...)
		return ctx.App.Run(args)
	}
	return ctx.App.Run(os.Args)
}

func flagHackLookup(flagName string) string {
	// e.g. "-d" for "--driver"
	flagPrefix := flagName[1:3]

	// TODO: Should we support -flag-name (single hyphen) syntax as well?
	for i, arg := range os.Args {
		if strings.Contains(arg, flagPrefix) {
			// format '--driver foo' or '-d foo'
			if arg == flagPrefix || arg == flagName {
				if i+1 < len(os.Args) {
					return os.Args[i+1]
				}
			}

			// format '--driver=foo' or '-d=foo'
			if strings.HasPrefix(arg, flagPrefix+"=") || strings.HasPrefix(arg, flagName+"=") {
				return strings.Split(arg, "=")[1]
			}
		}
	}

	return ""
}

type cliConfigGetter struct {
	name string
	ctx  *cli.Context
}

func (c cliConfigGetter) GetConfig() (rpcDriver.DriverOptions, error) {
	driverOpts := getDriverOpts(c.ctx)
	driverOpts.StringOptions["name"] = c.name
	return driverOpts, nil
}

type cliPersistStore struct{}

func (c cliPersistStore) Check(name string) (bool, error) {
	path := filepath.Join(utils.HomeDir(), "clusters", name)
	if _, err := os.Stat(filepath.Join(path, defaultConfigName)); os.IsNotExist(err) {
		return false, nil
	}
	cls := cluster.Cluster{}
	data, err := ioutil.ReadFile(filepath.Join(path, defaultConfigName))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, &cls); err != nil {
		return false, err
	}
	if cls.Status != cluster.Running {
		return false, nil
	}
	return true, nil
}

func (c cliPersistStore) Get(name string) (cluster.Cluster, error) {
	path := filepath.Join(utils.HomeDir(), "clusters", name)
	if _, err := os.Stat(filepath.Join(path, defaultConfigName)); os.IsNotExist(err) {
		return cluster.Cluster{}, fmt.Errorf("%s not found", name)
	}
	cls := cluster.Cluster{}
	data, err := ioutil.ReadFile(filepath.Join(path, defaultConfigName))
	if err != nil {
		return cluster.Cluster{}, err
	}
	if err := json.Unmarshal(data, &cls); err != nil {
		return cluster.Cluster{}, err
	}
	return cls, nil
}

func (c cliPersistStore) Store(cls cluster.Cluster) error {
	// store kube config file
	if err := storeConfig(cls); err != nil {
		return err
	}
	// store json config file
	fileDir := filepath.Join(utils.HomeDir(), "clusters", cls.Name)
	for k, v := range map[string]string{
		cls.RootCACert:        caPem,
		cls.ClientKey:         clientKey,
		cls.ClientCertificate: clientCert,
	} {
		data, err := base64.StdEncoding.DecodeString(k)
		if err != nil {
			return err
		}
		if err := utils.WriteToFile(data, filepath.Join(fileDir, v)); err != nil {
			return err
		}
	}
	data, err := json.Marshal(cls)
	if err != nil {
		return err
	}
	return utils.WriteToFile(data, filepath.Join(fileDir, defaultConfigName))
}

func (c cliPersistStore) PersistStatus(cluster cluster.Cluster, status string) error {
	fileDir := filepath.Join(utils.HomeDir(), "clusters", cluster.Name)
	cluster.Status = status
	data, err := json.Marshal(cluster)
	if err != nil {
		return err
	}
	return utils.WriteToFile(data, filepath.Join(fileDir, defaultConfigName))
}

func create(ctx *cli.Context) error {
	persistStore := cliPersistStore{}
	addr := ctx.GlobalString("plugin-listen-addr")
	name := ""
	if ctx.NArg() > 0 {
		name = ctx.Args().Get(0)
	}
	configGetter := cliConfigGetter{
		name: name,
		ctx:  ctx,
	}
	// first try to receive the cluster from disk
	// ingore the error as we only care if cluster.name is present
	clusterFrom, _ := persistStore.Get(os.Args[len(os.Args)-1])
	if clusterFrom.DriverName != "" {
		cls, err := cluster.FromCluster(&clusterFrom, addr, configGetter, persistStore)
		if err != nil {
			return err
		}
		return cls.Create()
	}
	// if cluster doesn't exist then we try to create a new one
	driverName := ctx.String("driver")
	if driverName == "" {
		logrus.Error("Driver name is required")
		return cli.ShowCommandHelp(ctx, "create")
	}

	cls, err := cluster.NewCluster(driverName, addr, name, configGetter, persistStore)
	if err != nil {
		return err
	}
	if cls.Name == "" {
		logrus.Error("Cluster name is required")
		return cli.ShowCommandHelp(ctx, "create")
	}
	return cls.Create()
}

func lookUpDebugFlag() bool {
	for _, arg := range os.Args {
		if arg == "--debug" {
			return true
		}
	}
	return false
}

func getDriverFlags(opts rpcDriver.DriverFlags) []cli.Flag {
	flags := []cli.Flag{}
	for k, v := range opts.Options {
		switch v.Type {
		case "int":
			val, err := strconv.Atoi(v.Value)
			if err != nil {
				val = 0
			}
			flags = append(flags, cli.Int64Flag{
				Name:  k,
				Usage: v.Usage,
				Value: int64(val),
			})
		case "string":
			flags = append(flags, cli.StringFlag{
				Name:  k,
				Usage: v.Usage,
				Value: v.Value,
			})
		case "stringSlice":
			flags = append(flags, cli.StringSliceFlag{
				Name:  k,
				Usage: v.Usage,
			})
		case "bool":
			flags = append(flags, cli.BoolFlag{
				Name:  k,
				Usage: v.Usage,
			})
		}
	}
	return flags
}
