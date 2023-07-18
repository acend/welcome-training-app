// Package main starts the simple server on port and serves HTML,
// CSS, and JavaScript to clients.
package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// templates parses the specified templates and caches the parsed results
// to help speed up response times.
var templates = template.Must(template.ParseFiles("./templates/base.html", "./templates/body.html"))

type Trainee struct {
	Username string
	Password string
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
func index() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		ctx := context.Background()

		config := ctrl.GetConfigOrDie()
		clientset := kubernetes.NewForConfigOrDie(config)

		var labelSelector = "acend-userconfig=true"
		if ls := os.Getenv("LABEL_SELECtOR"); ls != "" {
			labelSelector = ls
		}

		var usernameKey = "username"
		if unk := os.Getenv("SECRET_USERNAME_KEY"); unk != "" {
			usernameKey = unk
		}

		var passworkKey = "password"
		if pwk := os.Getenv("SECRET_PASSWORD_KEY"); v != "" {
			passworkKey = pwk
		}

		var passworkKey = "password"
		if pwk := os.Getenv("SECRET_PASSWORD_KEY"); v != "" {
			passworkKey = pwk
		}

		var clusterName = "training"
		if cn := os.Getenv("CLUSTER_NAME"); cn != "" {
			clusterName = cn
		}

		var clusterDomain = "cluster.acend.ch"
		if cd := os.Getenv("CLUSTER_DOMAIN"); cd != "" {
			clusterDomain = cd
		}

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
			var trainee = Trainee{
				Username: string(secret.Data[usernameKey]),
				Password: string(secret.Data[passworkKey]),
			}

			trainees = append(trainees, trainee)
		}

		data := map[string]interface{}{
			"clusterName":   clusterName,
			"clusterDomain": clusterDomain
			"trainees":      trainees,
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

func main() {
	mux := http.NewServeMux()
	mux.Handle("/public/", logging(public()))
	mux.Handle("/", logging(index()))

	port, ok := os.LookupEnv("PORT")
	if !ok {
		port = "8080"
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
