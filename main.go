package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

type Route struct {
	Host     string
	Upstream string
}

var (
	configTemplate = `{{range .}}
server {
    listen 80;
    server_name {{.Host}};
    location / {
        proxy_pass http://{{.Upstream}};
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_cache_bypass $http_upgrade;
        keepalive 64;
    }
}
{{end}}`
	routesMutex sync.Mutex
	routes      []Route
)

func main() {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	defer dockerClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Catch signals to gracefully terminate
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signalChan
		cancel()
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := updateRoutes(ctx, dockerClient); err != nil {
				fmt.Println("Error updating routes:", err)
			}
		}
	}
}

func updateRoutes(ctx context.Context, dockerClient *client.Client) error {
	containers, err := dockerClient.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return errors.Join(errors.New("Error listing containers"), err)
	}

	newRoutes := []Route{}
	for _, container := range containers {
		if len(container.Names) == 0 {
			continue
		}
		host, ok := container.Labels["reprox.host"]
		if !ok || host == "" {
			continue
		}
		newRoutes = append(newRoutes, Route{
			Host:     host,
			Upstream: strings.TrimPrefix(container.Names[0], "/"),
		})
	}

	routesMutex.Lock()
	defer routesMutex.Unlock()

	if !routesChanged(newRoutes) {
		return nil
	}
	routes = newRoutes

	if err := writeConfig(); err != nil {
		return errors.Join(errors.New("Error writing config"), err)
	}

	if err := reloadNginx(); err != nil {
		return errors.Join(errors.New("Error reloading Nginx"), err)
	}

	return nil
}

func routesChanged(newRoutes []Route) bool {
	if len(newRoutes) != len(routes) {
		return true
	}
	for i, route := range newRoutes {
		if route != routes[i] {
			return true
		}
	}
	return false
}

func writeConfig() error {
	tmpl, err := template.New("nginx").Parse(configTemplate)
	if err != nil {
		return errors.Join(errors.New("Error parsing template"), err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, routes); err != nil {
		return errors.Join(errors.New("Error executing template"), err)
	}

	if err := os.WriteFile("/etc/nginx/conf.d/apps.conf", buf.Bytes(), 0644); err != nil {
		return errors.Join(errors.New("Error writing config file"), err)
	}

	return nil
}

func reloadNginx() error {
	if err := executeCommand("nginx", "-s", "reload"); err != nil {
		return errors.Join(errors.New("Error reloading Nginx"), err)
	}
	return nil
}

func executeCommand(name string, args ...string) error {
	cmd := fmt.Sprintf("%s %s", name, strings.Join(args, " "))
	return syscall.Exec(cmd, append([]string{name}, args...), os.Environ())
}
