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
	"github.com/robfig/cron/v3"
)

type Route struct {
	Host     string
	Upstream string
}

var (
	configTemplate = `{{range .}}
server {
    listen 80;
    listen [::]:80;
    server_name {{.Host}};
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name {{.Host}};

    ssl_certificate /etc/letsencrypt/live/{{.Host}}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/{{.Host}}/privkey.pem;

    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers EECDH+AESGCM:EECDH+CHACHA20:EDH+AESGCM;
    ssl_prefer_server_ciphers on;
    ssl_session_cache shared:SSL:10m;
    ssl_session_timeout 1h;
    ssl_session_tickets off;

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

	c := cron.New()
	if _, err := c.AddFunc("0 0 * * *", CertbotRun); err != nil {
		panic(fmt.Sprintf("failed to add cron job: %v", err))
	}

	go func() {
		<-time.After(5 * time.Second)
		CertbotRun()
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

func CertbotRun() {
	if err := certbotRun(); err != nil {
		fmt.Println("Error running certbot:", err)
	} else {
		fmt.Println("Certbot run successful")
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

	// Ensure certificates are available for all routes
	for _, route := range routes {
		if err := ensureCertificate(route.Host); err != nil {
			return errors.Join(errors.New("Error ensuring certificate"), err)
		}
	}

	if err := writeConfig(); err != nil {
		return errors.Join(errors.New("Error writing config"), err)
	}

	if err := reloadNginx(); err != nil {
		return errors.Join(errors.New("Error reloading Nginx"), err)
	}

	// run certbot to request certificates for new routes
	if err := certbotRun(); err != nil {
		return errors.Join(errors.New("Error running certbot"), err)
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

	for _, route := range routes {
		if err := executeCommand(
			"certbot",
			"--nginx",
			"--non-interactive",
			"--agree-tos",
			"--email", certbotEmail,
			"--domains", route.Host,
		); err != nil {
			return errors.Join(errors.New("Error starting certbot"), err)
		}
	}

	return nil
}

// ensureCertificate ensures that a certificate is available for the given host.
// If certbot has not been run for the host, this function will generate a new self-signed certificate to make sure the server can start.
// If there is a certificate, it will do nothing.
func ensureCertificate(host string) error {
	if !exists("/etc/letsencrypt/live/"+host+"/fullchain.pem") || !exists("/etc/letsencrypt/live/"+host+"/privkey.pem") {
		// generate self-signed certificate using openssl that is valid for 1 hour
		if err := executeCommand(
			"openssl",
			"req",
			"-x509",
			"-newkey", "rsa:4096",
			"-keyout", "/etc/letsencrypt/live/"+host+"/privkey.pem",
			"-out", "/etc/letsencrypt/live/"+host+"/fullchain.pem",
			"-days", "1",
			"-nodes",
			"-subj", "/CN="+host,
		); err != nil {
			return errors.Join(errors.New("Error generating self-signed certificate"), err)
		}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
