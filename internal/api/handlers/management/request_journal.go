package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/requestjournal"
)

// GetRequestJournal lists durable request journal records (newest first).
// Query: limit (default 100), provider, supplier, failed=1
func (h *Handler) GetRequestJournal(c *gin.Context) {
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	failedOnly := c.Query("failed") == "1" || strings.EqualFold(c.Query("failed"), "true")
	records, err := requestjournal.List(requestjournal.ListOptions{
		Limit:      limit,
		Provider:   strings.TrimSpace(c.Query("provider")),
		Supplier:   strings.TrimSpace(c.Query("supplier")),
		FailedOnly: failedOnly,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"records":  records,
		"count":    len(records),
		"durable":  requestjournal.Enabled(),
		"location": "logs/request-journal",
	})
}

// DeleteRequestJournalRecord hard-deletes one record by id.
func (h *Handler) DeleteRequestJournalRecord(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}
	ok, err := requestjournal.DeleteID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "record not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "id": id})
}

// DeleteRequestJournal hard-deletes all durable request journal files.
func (h *Handler) DeleteRequestJournal(c *gin.Context) {
	n, err := requestjournal.DeleteAll()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "deleted", "files": n})
}
