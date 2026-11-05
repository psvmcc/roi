package roi

import (
	"database/sql"
	"net/http"
	"strconv"
)

func (a *App) adminAPIDevice(w http.ResponseWriter, r *http.Request) {
	var credentials APIDeviceCredentials
	var err error
	name := r.FormValue("device_name")
	if r.URL.Path == "/api-devices/create" {
		credentials, err = a.store.CreateAPIDevice(r.Context(), name)
	} else {
		id, e := strconv.ParseInt(r.FormValue("device"), 10, 64)
		if e != nil || id < 1 {
			http.Error(w, "invalid device", 400)
			return
		}
		var login string
		if err = a.store.DB.QueryRowContext(r.Context(), "SELECT name,login FROM api_devices WHERE id=?", id).Scan(&name, &login); err != nil {
			if err == sql.ErrNoRows {
				err = clientError("unknown API device")
			}
			a.adminStoreError(w, r, err)
			return
		}
		if identity, ok := r.Context().Value(sessionContextKey{}).(*requestIdentity); ok {
			identity.TargetDevice = name + " [" + login + "]"
		}
		switch r.URL.Path {
		case "/api-devices/code", "/api-devices/delete":
			if r.FormValue("confirm") != "device" {
				http.Error(w, "device action confirmation required", 400)
				return
			}
			if r.URL.Path == "/api-devices/code" {
				credentials, err = a.store.NewAPIDeviceCode(r.Context(), id)
			} else {
				err = a.store.DeleteAPIDevice(r.Context(), id)
			}
		case "/api-devices/rename":
			err = a.store.RenameAPIDevice(r.Context(), id, r.FormValue("device_name"))
		default:
			http.NotFound(w, r)
			return
		}
	}
	if err != nil {
		a.adminStoreError(w, r, err)
		return
	}
	if credentials.Code != "" {
		credentials.APIURL = a.config.readerAPIURL()
		if identity, ok := r.Context().Value(sessionContextKey{}).(*requestIdentity); ok && identity.TargetDevice == "" {
			identity.TargetDevice = name + " [" + credentials.Login + "]"
		}
		if wantsAdminJSON(r) {
			_ = jsonReply(w, map[string]any{"ok": true, "credentials": credentials, "message": "Save the one-time sign-in code. It will not be shown again."})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err = adminTemplate.ExecuteTemplate(w, "api-credentials.html", credentials); err != nil {
			a.requestError(w, r, err)
		}
		return
	}
	if wantsAdminJSON(r) {
		_ = jsonReply(w, map[string]any{"ok": true, "message": "API device updated."})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
