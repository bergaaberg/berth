//go:build windows

package main

import "fmt"

func run(cmd string, args []string) error {
	o, err := parseOpts(args)
	if err != nil {
		return err
	}
	switch cmd {
	case "setup":
		return setup(o)
	case "start":
		return start()
	case "stop":
		return stop()
	case "status":
		return status(o)
	case "use":
		return use()
	case "cli":
		return installCLI(true)
	case "update":
		return update()
	case "install":
		return installLogin()
	case "uninstall":
		return uninstallLogin()
	case "serve":
		return serve()
	case "log":
		return showLog()
	case "remove":
		return remove(o)
	case "version", "--version", "-v":
		fmt.Println("berth " + version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (berth help)", cmd)
}
