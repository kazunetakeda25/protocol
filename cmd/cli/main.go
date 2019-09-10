package main

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/Conscience/protocol/config"
	"github.com/Conscience/protocol/swarm/noderpc"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var ErrNotEnoughArgs = errors.New("not enough args")

func main() {
	app := cli.NewApp()

	app.Version = "0.0.1"
	app.Copyright = "(c) 2018 Conscience, Inc."
	app.Usage = "Utility for interacting with the Axon network"

	app.Commands = []cli.Command{
		{
			Name:      "faucet",
			Aliases:   []string{},
			UsageText: "axon faucet",
			Usage:     "request ETH from the faucet",
			ArgsUsage: "[args usage]",
			Action: func(c *cli.Context) error {
				client, err := getClient()
				if err != nil {
					return err
				}
				defer client.Close()

				ethAddr, err := client.EthAddress()
				if err != nil {
					return err
				}

				body, err := json.Marshal(struct {
					Address string  `json:"address"`
					Amount  float64 `json:"amount"`
				}{ethAddr, 1})
				if err != nil {
					return err
				}
				resp, err := http.Post("http://app.axon.science/api/faucet", "application/json", bytes.NewReader(body))
				if err != nil {
					return errors.WithStack(err)
				}
				defer resp.Body.Close()

				respBody, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					return errors.WithStack(err)
				}

				if resp.StatusCode > 399 {
					return errors.Errorf("unexpected error from faucet: %v", string(respBody))
				}
				fmt.Println("response:", string(respBody))
				return nil
			},
		},
		{
			Name:      "init",
			Aliases:   []string{"i"},
			UsageText: "axon init <repo ID>",
			Usage:     "initialize a git repo to interact with the Axon network",
			ArgsUsage: "[args usage]",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "name",
					Value: "",
					Usage: "name for local repo config",
				},
				cli.StringFlag{
					Name:  "email",
					Value: "",
					Usage: "email for local repo config",
				},
			},
			Action: func(c *cli.Context) error {
				repoID := c.Args().Get(0)
				if repoID == "" {
					return ErrNotEnoughArgs
				}
				path := c.Args().Get(1)
				if path == "" {
					cwd, err := os.Getwd()
					if err != nil {
						return err
					}
					path = cwd
				}
				name := c.String("name")
				email := c.String("email")
				return initRepo(repoID, path, name, email)
			},
		},
		{
			Name:      "import",
			Aliases:   []string{},
			UsageText: "axon import [--repoID=...] <path to repo>",
			Usage:     "import an existing git repo",
			ArgsUsage: "[args usage]",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "repoID",
					Value: "",
					Usage: "ID of the new repo on the network",
				},
			},
			Action: func(c *cli.Context) error {
				repoRoot := c.Args().Get(0)
				if repoRoot == "" {
					return ErrNotEnoughArgs
				}
				repoID := c.String("repoID")
				return importRepo(repoRoot, repoID)
			},
		},
		{
			Name:      "set-username",
			UsageText: "axon set-username <username>",
			Usage:     "set your username on the Axon network",
			ArgsUsage: "[args usage]",
			Action: func(c *cli.Context) error {
				if len(c.Args()) < 1 {
					return ErrNotEnoughArgs
				}

				username := c.Args().Get(0)
				return setUsername(username)
			},
		},
		{
			Name:      "set-chunking",
			UsageText: "axon set-chunking <filename> <on | off>",
			Usage:     "enable or disable file chunking for the given file",
			ArgsUsage: "[args usage]",
			Action: func(c *cli.Context) error {
				if len(c.Args()) < 2 {
					return ErrNotEnoughArgs
				}

				filename := c.Args().Get(0)
				enabledStr := c.Args().Get(1)

				var enabled bool
				if enabledStr == "on" {
					enabled = true
				} else if enabledStr == "off" {
					enabled = false
				} else {
					return errors.New("final parameter must be either 'on' or 'off'")
				}

				pwd, err := os.Getwd()
				if err != nil {
					return err
				}

				var repoRoot string
				for {
					_, err := os.Stat(filepath.Join(pwd, ".git"))
					if os.IsNotExist(err) {
						if pwd == "/" {
							return errors.New("you must call this command inside of a repository")
						} else {
							pwd = filepath.Dir(pwd)
							continue
						}
					} else if err != nil {
						return err
					} else {
						repoRoot = pwd
						break
					}
				}

				filename, err = filepath.Abs(filename)
				if err != nil {
					return err
				}
				filename, err = filepath.Rel(repoRoot, filename)
				if err != nil {
					return err
				}

				client, err := getClient()
				if err != nil {
					return err
				}
				defer client.Close()

				// @@TODO: give context a timeout and make it configurable
				return client.SetFileChunking(context.Background(), "", repoRoot, filename, enabled)
			},
		},
		// {
		// 	Name:      "replicate",
		// 	UsageText: "axon replicate <repo ID> <1 | 0>",
		// 	Usage:     "set whether or not to replicate the given repo",
		// 	ArgsUsage: "[args usage]",
		// 	Action: func(c *cli.Context) error {
		// 		if len(c.Args()) < 2 {
		// 			return ErrNotEnoughArgs
		// 		}

		// 		repoID := c.Args().Get(0)
		// 		_shouldReplicate := c.Args().Get(1)

		// 		shouldReplicate, err := strconv.ParseBool(_shouldReplicate)
		// 		if err != nil {
		// 			return errors.New("Bad argument.  Must be 1 or 0.")
		// 		}
		// 		return setReplicationPolicy(repoID, shouldReplicate)
		// 	},
		// },
		{
			Name:      "repos",
			UsageText: "axon repos",
			Usage:     "returns a list of axon repositories hosted locally on this machine",
			ArgsUsage: "[args usage]",
			Action: func(c *cli.Context) error {
				repos, err := getLocalRepos()
				if err != nil {
					return err
				}
				for _, repo := range repos {
					fmt.Fprintf(c.App.Writer, "%s\n", repo)
				}

				return nil
			},
		},
		{
			Name:      "get-refs",
			UsageText: "axon get-refs <repo ID>",
			Usage:     "return all on-chain refs for the given repo",
			ArgsUsage: "[args usage]",
			Action: func(c *cli.Context) error {
				if len(c.Args()) < 1 {
					return ErrNotEnoughArgs
				}

				repoID := c.Args().Get(0)

				refs, err := getAllRefs(repoID)
				if err != nil {
					return err
				}
				for _, ref := range refs {
					fmt.Fprintf(c.App.Writer, "%s %s\n", ref.CommitHash, ref.RefName)
				}

				return nil
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v", err)
	}
}

func getClient() (*noderpc.Client, error) {
	cfg, err := config.ReadConfig()
	if err != nil {
		return nil, err
	}
	return noderpc.NewClient(cfg.RPCClient.Host)
}
