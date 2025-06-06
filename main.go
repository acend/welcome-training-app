// Package main starts the simple server on port and serves HTML,
// CSS, and JavaScript to clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// templates parses the specified templates and caches the parsed results
// to help speed up response times.
var templates = template.Must(template.ParseFiles("./templates/base.html", "./templates/body.html", "./templates/login.html"))

var labelSelector = "acend-userconfig=true"
var usernameKey = "username"
var passwordKey = "password"
var clusterName = "training"
var clusterDomain = "cluster.acend.ch"
var token = ""

type Trainee struct {
	Username    string
	Password    string
	DisplayName string // New field for the display name
	IsAdmin     bool
	PodReady    bool // New field for pod status
}

// logging is middleware for wrapping any handler we want to track response
// times for and to see what resources are requested.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		req := fmt.Sprintf("%s %s", r.Method, r.URL)
		log.Println(req)
		next.ServeHTTP(w, r)
		log.Println(req, "completed in", time.Now().Sub(start))
	})
}

// index is the handler responsible for rending the index page for the site.
func index(teacher bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if !teacher {
			t := r.URL.Query().Get("token")
			if token != "" && t != token {
				if err := templates.ExecuteTemplate(w, "login", nil); err != nil {
					http.Error(w, fmt.Sprintf("index: couldn't parse template: %v", err), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
		}

		ctx := context.Background()
		config := ctrl.GetConfigOrDie()
		clientset := kubernetes.NewForConfigOrDie(config)

		// Define the options to list secrets
		listOptions := metav1.ListOptions{
			LabelSelector: labelSelector,
		}

		secretList, err := clientset.CoreV1().Secrets("").List(ctx, listOptions)
		if err != nil {
			panic(err.Error())
		}

		var trainees []Trainee
		for _, secret := range secretList.Items {
			username := string(secret.Data[usernameKey])
			isAdmin := false

			_, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, "cluster-admin-"+username, metav1.GetOptions{})

			if err == nil {
				if !teacher {
					// ClusterRoleBinding exists, skip this trainee for non-teacher
					continue
				} else {
					// ClusterRoleBinding exists, mark as admin for teacher view
					isAdmin = true
				}
			}

			podReady := false
			if teacher {
				// Check pod status for {username}-webshell using label selector
				labelSelector := fmt.Sprintf("app.kubernetes.io/instance=%s-webshell", username)
				pods, err := clientset.CoreV1().Pods(username).List(ctx, metav1.ListOptions{
					LabelSelector: labelSelector,
				})
				if err == nil {
					for _, pod := range pods.Items {
						for _, cs := range pod.Status.ContainerStatuses {
							if cs.Ready {
								podReady = true
								break
							}
						}
						if podReady {
							break
						}
					}
				}
			}

			trainee := Trainee{
				Username:    username,
				Password:    string(secret.Data[passwordKey]),
				DisplayName: getDisplayName(clientset, ctx, username),
				IsAdmin:     isAdmin,
				PodReady:    podReady,
			}
			trainees = append(trainees, trainee)
		}

		// Sort trainees by the numeric part of the username (e.g., user1, user2, ...)
		sort.Slice(trainees, func(i, j int) bool {
			getNum := func(username string) int {
				if strings.HasPrefix(username, "user") {
					numStr := username[4:]
					num, err := strconv.Atoi(numStr)
					if err == nil {
						return num
					}
				}
				return 0
			}
			return getNum(trainees[i].Username) < getNum(trainees[j].Username)
		})

		data := map[string]interface{}{
			"clusterName":   clusterName,
			"clusterDomain": clusterDomain,
			"trainees":      trainees,
			"teacher":       teacher,
			"token":         token,
		}

		err2 := templates.ExecuteTemplate(w, "base", &data)
		if err2 != nil {
			http.Error(w, fmt.Sprintf("index: couldn't parse template: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// public serves static assets such as CSS and JavaScript to clients.
func public() http.Handler {
	return http.StripPrefix("/public/", http.FileServer(http.Dir("./public")))
}

func health() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// WebSocket hub for broadcasting trainee name updates and input locks
var wsHub = newHub()

type hub struct {
	clients      map[*websocket.Conn]bool
	broadcast    chan map[string]string
	register     chan *websocket.Conn
	unregister   chan *websocket.Conn
	locks        map[string]bool // username -> locked
	locksChanged chan struct{}
}

func newHub() *hub {
	h := &hub{
		clients:      make(map[*websocket.Conn]bool),
		broadcast:    make(chan map[string]string),
		register:     make(chan *websocket.Conn),
		unregister:   make(chan *websocket.Conn),
		locks:        make(map[string]bool),
		locksChanged: make(chan struct{}, 1),
	}
	go h.run()
	return h
}

func (h *hub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			// Send current locks to new client
			locksMsg := map[string]interface{}{"type": "locks", "locks": h.locks}
			client.WriteJSON(locksMsg)
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				client.Close()
				delete(h.clients, client)
			}
		case msg := <-h.broadcast:
			for client := range h.clients {
				client.WriteJSON(msg)
			}
		case <-h.locksChanged:
			locksMsg := map[string]interface{}{"type": "locks", "locks": h.locks}
			for client := range h.clients {
				client.WriteJSON(locksMsg)
			}
		}
	}
}

func wsTraineeNamesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		wsHub.register <- conn
		defer func() { wsHub.unregister <- conn }()
		for {
			type wsMsg struct {
				Type     string `json:"type"`
				Username string `json:"username"`
			}
			_, msgBytes, err := conn.ReadMessage()
			if err != nil {
				break
			}
			var msg wsMsg
			_ = json.Unmarshal(msgBytes, &msg)
			if msg.Type == "lock" {
				wsHub.locks[msg.Username] = true
				select {
				case wsHub.locksChanged <- struct{}{}:
				default:
				}
			} else if msg.Type == "unlock" {
				delete(wsHub.locks, msg.Username)
				select {
				case wsHub.locksChanged <- struct{}{}:
				default:
				}
			}
		}
	})
}

// Helper to broadcast all trainee names to all websocket clients
func broadcastTraineeNames() {
	ctx := context.Background()
	config := ctrl.GetConfigOrDie()
	clientset := kubernetes.NewForConfigOrDie(config)
	listOptions := metav1.ListOptions{LabelSelector: labelSelector}
	secretList, err := clientset.CoreV1().Secrets("").List(ctx, listOptions)
	if err != nil {
		return
	}
	names := map[string]string{}
	for _, secret := range secretList.Items {
		username := string(secret.Data[usernameKey])
		displayName := getDisplayName(clientset, ctx, username)
		names[username] = displayName
	}
	wsHub.broadcast <- names
}

// Handler to update trainee display name (persist as ConfigMap)
func updateTraineeNameHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		username := r.URL.Query().Get("username")
		if username == "" {
			http.Error(w, "Missing username", http.StatusBadRequest)
			return
		}
		name := r.URL.Query().Get("name")

		ctx := context.Background()
		config := ctrl.GetConfigOrDie()
		clientset := kubernetes.NewForConfigOrDie(config)

		cmName := "trainee-displayname-" + username
		cmNamespace := username // Store in user's namespace
		cmData := map[string]string{"displayName": name}

		cm, err := clientset.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			// Update existing
			cm.Data = cmData
			_, err = clientset.CoreV1().ConfigMaps(cmNamespace).Update(ctx, cm, metav1.UpdateOptions{})
		} else {
			// Create new
			cm := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: cmName,
				},
				Data: cmData,
			}
			_, err = clientset.CoreV1().ConfigMaps(cmNamespace).Create(ctx, cm, metav1.CreateOptions{})
		}
		if err != nil {
			http.Error(w, "Failed to save name: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		go broadcastTraineeNames() // Broadcast after update
	})
}

// Handler to return all trainee display names as JSON
func getTraineeNamesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		config := ctrl.GetConfigOrDie()
		clientset := kubernetes.NewForConfigOrDie(config)

		// List all trainees (secrets)
		listOptions := metav1.ListOptions{
			LabelSelector: labelSelector,
		}
		secretList, err := clientset.CoreV1().Secrets("").List(ctx, listOptions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Build map of username -> displayName
		names := map[string]string{}
		for _, secret := range secretList.Items {
			username := string(secret.Data[usernameKey])
			displayName := getDisplayName(clientset, ctx, username)
			names[username] = displayName
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(names)
	})
}

// Handler to return lab progress for a given username
func getLabProgressHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := r.URL.Query().Get("username")
		if username == "" {
			http.Error(w, "Missing username", http.StatusBadRequest)
			return
		}
		url := fmt.Sprintf("https://example-web-app-%s.training.%s/progress", username, clusterDomain)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}
		req.Header.Set("flat", "true")
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error": "progress application not ready"}`))
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
	})
}

// Helper to get display name from ConfigMap
func getDisplayName(clientset *kubernetes.Clientset, ctx context.Context, username string) string {
	cmName := "trainee-displayname-" + username
	cmNamespace := username
	cm, err := clientset.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil || cm.Data == nil {
		return ""
	}
	return cm.Data["displayName"]
}

func main() {
	mux := http.NewServeMux()
	mux.Handle("/health/", logging(health()))
	mux.Handle("/public/", logging(public()))
	mux.Handle("/", logging(index(false)))
	mux.Handle("/teacher", logging(index(true)))
	mux.Handle("/api/trainee-name", logging(updateTraineeNameHandler())) // New API endpoint
	mux.Handle("/api/trainee-names", logging(getTraineeNamesHandler()))  // New polling endpoint
	mux.Handle("/ws/trainee-names", wsTraineeNamesHandler())             // WebSocket endpoint
	mux.Handle("/api/lab-progress", logging(getLabProgressHandler()))

	port, ok := os.LookupEnv("PORT")
	if !ok {
		port = "8080"
	}

	if ls := os.Getenv("LABEL_SELECTOR"); ls != "" {
		labelSelector = ls
	}

	if unk := os.Getenv("SECRET_USERNAME_KEY"); unk != "" {
		usernameKey = unk
	}

	if pwk := os.Getenv("SECRET_PASSWORD_KEY"); pwk != "" {
		passwordKey = pwk
	}

	if cn := os.Getenv("CLUSTER_NAME"); cn != "" {
		clusterName = cn
	}

	if t := os.Getenv("TOKEN"); t != "" {
		token = t
	}

	if cd := os.Getenv("CLUSTER_DOMAIN"); cd != "" {
		clusterDomain = cd
	}

	addr := fmt.Sprintf(":%s", port)
	server := http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  15 * time.Second,
	}
	log.Println("main: running simple server on port", port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("main: couldn't start simple server: %v\n", err)
	}
}
