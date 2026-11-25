package admin

import (
	"errors"
	"net/http"
	"strings"

	"quickrelay-studio/internal/updater"
)

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	res, err := s.updater.Check(r.Context(), mode)
	if err != nil {
		var checkErr *updater.CheckFailedError
		if errors.As(err, &checkErr) {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]any{
					"code":    "update_check_failed",
					"message": checkErr.Error(),
				},
				"attempts": checkErr.Attempts,
				"mode":     checkErr.Mode,
				"repo":     checkErr.Repo,
			})
			return
		}
		writeError(w, http.StatusBadGateway, "update_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	var req updater.DownloadRequest
	if !decodeJSON(w, r, &req, 32<<10) {
		return
	}
	if strings.TrimSpace(req.DownloadURL) == "" {
		res, err := s.updater.Check(r.Context(), req.Mode)
		if err != nil {
			writeError(w, http.StatusBadGateway, "update_check_failed", err.Error())
			return
		}
		if strings.TrimSpace(res.DownloadURL) == "" {
			writeError(w, http.StatusNotFound, "update_asset_missing", "最新发布版本未提供可供下载的安装包资产")
			return
		}
		req.Tag = res.Tag
		req.DownloadURL = res.DownloadURL
		req.AssetName = res.AssetName
		if req.SHA256 == "" {
			req.SHA256 = res.SHA256
		}
	}

	status, err := s.updater.StartDownload(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "update_download_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.updater.Status())
}
