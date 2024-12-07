package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
    http2 on;
    location / {
        proxy_pass http://{{.Upstream}};
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection 'upgrade';
        proxy_set_header Host $host;
        proxy_cache_bypass $http_upgrade;
    }
}
{{end}}`
	routesMutex  sync.Mutex
	routes       []Route
	certbotEmail string = os.Getenv("CERTBOT_EMAIL")
)

func main() {
	if err := startNginx(); err != nil {
		panic(err)
	}
	defer stopNginx()

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

	if err := updateRoutes(ctx, dockerClient); err != nil {
		fmt.Println("Error updating routes:", err)
	}

	go func() {
		<-time.After(5 * time.Second)
		for {
			if err := certbotRun(); err != nil {
				fmt.Println("Error running certbot:", err)
				<-time.After(5 * time.Minute)
			} else {
				<-time.After(24 * time.Hour)
			}
		}
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
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Join(errors.New("Error executing command"), errors.New(string(out)), err)
	}
	return nil
}

func startNginx() error {
	nginxCmd := exec.Command("nginx")
	nginxCmd.Stdout = os.Stdout
	nginxCmd.Stderr = os.Stderr
	if err := nginxCmd.Run(); err != nil {
		return errors.Join(errors.New("Error starting Nginx"), err)
	}
	return nil
}

func stopNginx() error {
	if err := executeCommand("nginx", "-s", "stop"); err != nil {
		return errors.Join(errors.New("Error stopping Nginx"), err)
	}
	return nil
}

func certbotRun() error {
	if len(routes) == 0 {
		return errors.New("No routes to request certificates for")
	}

	if err := executeCommand(
		"certbot",
		"--nginx",
		"--non-interactive",
		"--agree-tos",
		"--email", certbotEmail,
		"--domains", strings.Join(mapSlice(routes, func(route Route) string { return route.Host }), ","),
	); err != nil {
		return errors.Join(errors.New("Error starting certbot"), err)
	}
	return nil
}

func mapSlice[T any, U any](s []T, f func(T) U) []U {
	result := make([]U, len(s))
	for i, v := range s {
		result[i] = f(v)
	}
	return result
}
