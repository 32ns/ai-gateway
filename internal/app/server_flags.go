package app

import (
	"flag"
	"io"

	"github.com/32ns/ai-gateway/internal/config"
)

type ServerFlags struct {
	ConfigPath string
	Host       string
	Port       string
}

func ParseConfigPathFlag(args []string) (string, error) {
	parsed, err := ParseServerFlags(args)
	if err != nil {
		return "", err
	}
	return parsed.ConfigPath, nil
}

func ParseServerFlags(args []string) (ServerFlags, error) {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", config.DefaultPath, "config file path")
	host := flags.String("host", "", "listen host override")
	port := flags.String("port", "", "listen port override")
	if err := flags.Parse(args); err != nil {
		return ServerFlags{}, err
	}
	if err := rejectUnexpectedArgs(flags); err != nil {
		return ServerFlags{}, err
	}
	return ServerFlags{ConfigPath: *configPath, Host: *host, Port: *port}, nil
}
