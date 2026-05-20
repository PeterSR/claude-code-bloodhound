//go:build linux && gui

package main

/*
#cgo pkg-config: gtk+-3.0 webkit2gtk-4.1
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>
#include <stdlib.h>
#include <string.h>

// One-window app. Pointers are package-private state; main is single-
// threaded around GTK so guarding them with locks is unnecessary.
static GtkWidget *bh_window = NULL;
static WebKitWebView *bh_view = NULL;

static void bh_destroy_cb(GtkWidget *w, gpointer data) {
    (void)w;
    (void)data;
    gtk_main_quit();
}

static void bh_init(void) {
    gtk_init_check(NULL, NULL);
}

static void bh_set_prgname(const char *name) {
    g_set_prgname(name);
    g_set_application_name(name);
}

static void bh_create_window(const char *title, int w, int h, const char *icon_name) {
    bh_window = gtk_window_new(GTK_WINDOW_TOPLEVEL);
    gtk_window_set_title(GTK_WINDOW(bh_window), title);
    gtk_window_set_default_size(GTK_WINDOW(bh_window), w, h);
    if (icon_name != NULL && *icon_name != '\0') {
        gtk_window_set_icon_name(GTK_WINDOW(bh_window), icon_name);
    }
    g_signal_connect(bh_window, "destroy", G_CALLBACK(bh_destroy_cb), NULL);
    bh_view = WEBKIT_WEB_VIEW(webkit_web_view_new());
    WebKitSettings *settings = webkit_web_view_get_settings(bh_view);
    webkit_settings_set_enable_smooth_scrolling(settings, TRUE);
    webkit_settings_set_hardware_acceleration_policy(settings, WEBKIT_HARDWARE_ACCELERATION_POLICY_ALWAYS);
    gtk_container_add(GTK_CONTAINER(bh_window), GTK_WIDGET(bh_view));
    gtk_widget_show_all(bh_window);
}

static void bh_navigate(const char *url) {
    if (bh_view == NULL) return;
    webkit_web_view_load_uri(bh_view, url);
}

static void bh_run(void) { gtk_main(); }

// Marshalling raise / quit requests from non-GUI goroutines onto the GUI
// thread. g_idle_add is safe to call from any thread; it queues the
// callback to run inside gtk_main.

static gboolean bh_idle_raise(gpointer data) {
    char *token = (char *)data;
    if (bh_window != NULL) {
        if (token != NULL && *token != '\0') {
            gtk_window_set_startup_id(GTK_WINDOW(bh_window), token);
        }
        gtk_window_present(GTK_WINDOW(bh_window));
    }
    if (token != NULL) g_free(token);
    return G_SOURCE_REMOVE;
}

static void bh_request_raise(const char *token) {
    char *copy = NULL;
    if (token != NULL && *token != '\0') copy = g_strdup(token);
    g_idle_add(bh_idle_raise, copy);
}

static gboolean bh_idle_quit(gpointer data) {
    (void)data;
    if (bh_window != NULL) {
        gtk_widget_destroy(bh_window);
    } else {
        gtk_main_quit();
    }
    return G_SOURCE_REMOVE;
}

static void bh_request_quit(void) {
    g_idle_add(bh_idle_quit, NULL);
}
*/
import "C"

import (
	"unsafe"
)

func gtkInit()           { C.bh_init() }
func gtkRun()            { C.bh_run() }
func gtkRequestQuit()    { C.bh_request_quit() }
func gtkRequestRaise(token string) {
	var ctok *C.char
	if token != "" {
		ctok = C.CString(token)
		defer C.free(unsafe.Pointer(ctok))
	}
	C.bh_request_raise(ctok)
}
func setPrgName(name string) {
	cs := C.CString(name)
	defer C.free(unsafe.Pointer(cs))
	C.bh_set_prgname(cs)
}
func createWindow(title string, width, height int, iconName string) {
	ct := C.CString(title)
	ci := C.CString(iconName)
	defer C.free(unsafe.Pointer(ct))
	defer C.free(unsafe.Pointer(ci))
	C.bh_create_window(ct, C.int(width), C.int(height), ci)
}
func navigate(url string) {
	cs := C.CString(url)
	defer C.free(unsafe.Pointer(cs))
	C.bh_navigate(cs)
}
