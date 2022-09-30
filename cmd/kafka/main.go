package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/redhatinsights/edge-api/config"
	l "github.com/redhatinsights/edge-api/logger"
	"github.com/redhatinsights/edge-api/pkg/clients/imagebuilder"
	kafkacommon "github.com/redhatinsights/edge-api/pkg/common/kafka"
	"github.com/redhatinsights/edge-api/pkg/db"
	"github.com/redhatinsights/edge-api/pkg/models"
	"github.com/redhatinsights/edge-api/pkg/routes/common"
	"github.com/redhatinsights/platform-go-middlewares/identity"
	log "github.com/sirupsen/logrus"
)

// NOTE: this is currently designed for a single ibvents replica

// LoopTime interrupt query loop sleep time in minutes
// TODO: make this configurable in minutes
const (
	//	LoopTime = 1 * time.Minute
	LoopTime             = 50 * time.Second
	StaleBuildTime       = 6
	StaleInterruptedTime = 3
)

// ImageInProcess represents the DB View for images currently in build
type ImageInProcess struct {
	ImageID            uint
	ImageRequestID     string
	ImageOrgID         string
	ImageAccount       string
	ImageName          string `json:"image_name"`
	ImageStatus        string
	CommitStatus       string
	RepoStatus         string
	InstallerStatus    string
	ImageCreatedAt     time.Time
	ImageUpdatedAt     time.Time
	CommitID           uint
	CommitJobID        string
	CommitCreatedAt    time.Time
	CommitUpdatedAt    time.Time
	CommitURL          string
	ReposID            uint
	ReposCreatedAt     time.Time
	ReposUpdatedAt     time.Time
	RepoURL            string
	InstallerID        uint64
	InstallerJobID     string
	InstallerCreatedAt time.Time
	InstallerUpdatedAt time.Time
	InstallerISOURL    string
}

const (
	composeStatusBuilding    string = "building"
	composeStatusFailure     string = "failure"
	composeStatusPending     string = "pending"
	composeStatusRegistering string = "registering"
	composeStatusSuccess     string = "success"
	composeStatusUploading   string = "uploading"
)

// Compose is the Image Builder reference for commit or installer
type Compose struct {
	OrgID    string
	Account  string
	JobID    string
	Type     string
	Status   string
	IBClient *imagebuilder.Client
}

// CreateComposeFromImage accepts an image and returns a compose
func CreateComposeFromImage(ctx context.Context, image models.Image) Compose {
	compose := Compose{
		OrgID:   image.OrgID,
		Account: image.Account,
	}

	if image.HasOutputType(models.ImageTypeInstaller) && image.Installer.Status != "" {
		compose.Type = models.ImageTypeInstaller
		compose.JobID = image.Installer.ComposeJobID
	} else if image.Commit.ComposeJobID != "" {
		compose.Type = models.ImageTypeCommit
		compose.JobID = image.Commit.ComposeJobID
	}

	// init the Image Builder client with the new context and log
	// TODO: sync log fields with pre-EDA logging
	iblog := log.WithField("app", "edgemgmt-utility")
	compose.IBClient = imagebuilder.InitClient(ctx, iblog)

	return compose
}

// GetStatus returns an updated status from Image Builder
func (c *Compose) GetStatus() (string, error) {
	var composeStatus *imagebuilder.ComposeStatus
	var err error

	composeStatus, err = c.IBClient.GetComposeStatus(c.JobID)
	if err != nil {
		return "", err
	}

	status := composeStatus.ImageStatus.Status

	return string(status), nil
}

// get images with a build of x status and older than y hours
func getStaleBuilds(status string, age int) []models.Image {
	var images []models.Image

	// looks like we ran into a known pgx issue when using ? for parameters in certain prepared SQL statements
	// 		using Sprintf to predefine the query and pass to Where
	query := fmt.Sprintf("status = '%s' AND updated_at < NOW() - INTERVAL '%d hours'", status, age)
	qresult := db.DB.Debug().Where(query).Find(&images)
	if qresult.Error != nil {
		log.WithField("error", qresult.Error.Error()).Error("Stale builds query failed")
		return nil
	}

	if qresult.RowsAffected > 0 {
		log.WithFields(log.Fields{
			"numImages": qresult.RowsAffected,
			"status":    status,
			"interval":  age,
		}).Debug("Found stale image(s) with interval")
	}

	return images
}

// get images with a specific build status
// NOTE: this is a common function to return images across ALL Org IDs
func getImagesWithStatus(status string) []models.Image {
	var images []models.Image

	// looks like we ran into a known pgx issue when using ? for parameters in certain prepared SQL statements
	// 		using Sprintf to predefine the query and pass to Where
	qresult := db.DB.Debug().Joins("Commit").Joins("Installer").Where(fmt.Sprintf("Images.Status = '%s'", status)).Find(&images)

	if qresult.Error != nil {
		log.WithField("error", qresult.Error.Error()).Error("Image status query failed")
		return nil
	}

	if qresult.RowsAffected > 0 {
		log.WithFields(log.Fields{
			"numImages": qresult.RowsAffected,
			"status":    status,
		}).Debug("Found image(s) with status")
	}

	return images
}

// get images with a specific build status
// NOTE: this is a common function to return images across ALL Org IDs
func getPendingBuildingImages() []models.Image {
	var images []models.Image

	// looks like we ran into a known pgx issue when using ? for parameters in certain prepared SQL statements
	// 		using Sprintf to predefine the query and pass to Where
	qresult := db.DB.
		//Debug().
		Joins("Commit").Joins("Installer").
		Where(fmt.Sprintf("Images.Status = '%s' OR Images.Status = '%s'", models.ImageStatusPending, models.ImageStatusBuilding)).
		Find(&images)

	if qresult.Error != nil {
		log.WithField("error", qresult.Error.Error()).Error("Image status query failed")
		return nil
	}

	if qresult.RowsAffected > 0 {
		log.WithField("numImages", qresult.RowsAffected).Debug("Found pending/building images")
	}

	return images
}

// set the status for a specific image
func setImageStatus(id uint, status string) error {
	tx := db.DB.Debug().Model(&models.Image{}).Where("ID = ?", id).Update("Status", status)
	if tx.Error != nil {
		log.WithField("error", tx.Error.Error()).Error("Error updating image status")
		return tx.Error
	}

	log.WithField("imageID", id).Debug("Image updated with " + fmt.Sprint(status) + " status")

	return nil
}

func main() {
	// create a new context
	ctx := context.Background()

	// set things up
	log.Info("Starting up...")

	var images []models.Image

	// IBevent represents the struct of the value in a Kafka message
	// TODO: add the original requestid
	type IBevent struct {
		ImageID uint `json:"image_id"`
	}

	config.Init()
	l.InitLogger()
	cfg := config.Get()
	log.WithFields(log.Fields{
		"Hostname":                 cfg.Hostname,
		"Auth":                     cfg.Auth,
		"WebPort":                  cfg.WebPort,
		"MetricsPort":              cfg.MetricsPort,
		"LogLevel":                 cfg.LogLevel,
		"Debug":                    cfg.Debug,
		"BucketName":               cfg.BucketName,
		"BucketRegion":             cfg.BucketRegion,
		"RepoTempPath ":            cfg.RepoTempPath,
		"OpenAPIFilePath ":         cfg.OpenAPIFilePath,
		"ImageBuilderURL":          cfg.ImageBuilderConfig.URL,
		"InventoryURL":             cfg.InventoryConfig.URL,
		"PlaybookDispatcherConfig": cfg.PlaybookDispatcherConfig.URL,
		"TemplatesPath":            cfg.TemplatesPath,
		"DatabaseType":             cfg.Database.Type,
		"DatabaseName":             cfg.Database.Name,
		"DatabaseHost":             cfg.Database.Hostname,
		"DatabasePort":             cfg.Database.Port,
		"EdgeAPIURL":               cfg.EdgeAPIBaseURL,
		"EdgeAPIServiceHost":       cfg.EdgeAPIServiceHost,
		"EdgeAPIServicePort":       cfg.EdgeAPIServicePort,
	}).Info("Configuration Values:")
	db.InitDB()

	//iblog := log.WithField("app", "edgemgmt-utility")
	//ibClient := imagebuilder.InitClient(context.Background(), iblog)

	log.Info("Entering the infinite loop...")
	for {
		log.Debug("Sleeping...")
		// TODO: make this configurable
		//time.Sleep(LoopTime)
		// TODO: programatic method to avoid resuming a build until app is up or on way up???

		// handle stale interrupted builds not complete after StaleInterruptedTime hours
		staleInterruptedImages := getStaleBuilds(models.ImageStatusInterrupted, StaleInterruptedTime)
		for _, staleImage := range staleInterruptedImages {
			log.WithFields(log.Fields{
				"UpdatedAt": staleImage.UpdatedAt,
				"ID":        staleImage.ID,
				"Status":    staleImage.Status,
			}).Info("Processing stale interrupted image")

			statusUpdateError := setImageStatus(staleImage.ID, models.ImageStatusError)
			if statusUpdateError != nil {
				log.Error("Failed to update stale interrupted image build status")
			}
		}

		// handle stale builds not complete after StaleBuildTime hours
		staleBuildingImages := getStaleBuilds(models.ImageStatusBuilding, StaleBuildTime)
		for _, staleImage := range staleBuildingImages {
			log.WithFields(log.Fields{
				"UpdatedAt": staleImage.UpdatedAt.Time.Local().String(),
				"ID":        staleImage.ID,
				"Status":    staleImage.Status,
			}).Info("Processing stale building image")

			statusUpdateError := setImageStatus(staleImage.ID, models.ImageStatusError)
			if statusUpdateError != nil {
				log.Error("Failed to update stale building image build status")
			}
		}

		// handle image builds in INTERRUPTED status
		//	this is meant to handle builds that are interrupted when they are interrupted
		// 	the stale interrupted build routine (above) should never actually find anything while this is running
		qresult := db.DB.Debug().Where(&models.Image{Status: models.ImageStatusInterrupted}).Find(&images)
		if qresult.RowsAffected > 0 {
			log.WithField("numImages", qresult.RowsAffected).Info("Found image(s) with interrupted status")
		}

		for _, image := range images {
			log.WithFields(log.Fields{
				"imageID":   image.ID,
				"Account":   image.Account,
				"OrgID":     image.OrgID,
				"RequestID": image.RequestID,
			}).Info("Processing interrupted image")

			// we have a choice here...
			// 1. Send an event and a consumer on Edge API calls the resume.
			// 2. Send an API call to Edge API to call the resume.
			//
			// Currently using the API call.

			// send an API request
			// form the internal API call from env vars and add the original requestid
			url := fmt.Sprintf("http://%s:%d/api/edge/v1/images/%d/resume", cfg.EdgeAPIServiceHost, cfg.EdgeAPIServicePort, image.ID)
			log.WithField("apiURL", url).Debug("Created the api url string")
			req, _ := http.NewRequest("POST", url, nil)
			req.Header.Add("Content-Type", "application/json")

			// recreate a stripped down identity header
			strippedIdentity := fmt.Sprintf(`{ "identity": {"account_number": "%s", "org_id": "%s", "type": "User", "internal": {"org_id": "%s"} } }`, image.Account, image.OrgID, image.OrgID)
			log.WithField("identity_text", strippedIdentity).Debug("Creating a new stripped identity")
			base64Identity := base64.StdEncoding.EncodeToString([]byte(strippedIdentity))
			log.WithField("identity_base64", base64Identity).Debug("Using a base64encoded stripped identity")
			req.Header.Add("x-rh-identity", base64Identity)

			// create a client and send a request against the Edge API
			client := &http.Client{}
			res, err := client.Do(req)
			if err != nil {
				var code int
				if res != nil {
					code = res.StatusCode
				}
				log.WithFields(log.Fields{
					"statusCode": code,
					"error":      err,
				}).Error("Edge API resume request error")
			} else {
				respBody, err := ioutil.ReadAll(res.Body)
				if err != nil {
					log.Error("Error reading body of uninterrupted build resume response")
				}
				log.WithFields(log.Fields{
					"statusCode":   res.StatusCode,
					"responseBody": string(respBody),
					"error":        err,
				}).Debug("Edge API resume response")
				err = res.Body.Close()
				if err != nil {
					log.Error("Error closing body")
				}
			}

			// removed the old event producer and will replace with new Producer code when ready to move away from API call
			// it goes here
		}

		// TRYING THIS WITH POSTGRESQL VIEWS
		imagesInProcessQuery := `SELECT * FROM images_in_process`
		var imagesInProcess []ImageInProcess
		db.DB.Debug().Raw(imagesInProcessQuery).Scan(&imagesInProcess)
		for _, iip := range imagesInProcess {
			log.WithFields(log.Fields{"image_name": iip.ImageName,
				"image_id":         iip.ImageID,
				"image_status":     iip.ImageStatus,
				"commit_status":    iip.CommitStatus,
				"repo_status":      iip.RepoStatus,
				"installer_status": iip.InstallerStatus}).Debug("Image is in process")

			// recreate the identity and context so pre-EDA service code will work when called
			// NOTE: this has to be done for each image inside the images loop!
			identity, base64Identity := recreateIdentities(iip.ImageOrgID, iip.ImageAccount)

			// add the new identity to the context
			ctx = common.SetOriginalIdentity(ctx, base64Identity)

			// init the Image Builder client with the new context and log
			// TODO: sync log fields with pre-EDA logging
			iblog := log.WithField("microservice", "edgemgmt-utility")
			ibClient := imagebuilder.InitClient(ctx, iblog)

			// create common payload for ImageInstallerCreated events
			edgePayload := &models.EdgeImageComposeCreatedEventPayload{
				EdgeBasePayload: models.EdgeBasePayload{
					Identity:       identity,
					LastHandleTime: time.Now().Format(time.RFC3339),
					RequestID:      iip.ImageRequestID,
				},
				EdgeComposeBasePayload: models.EdgeComposeBasePayload{
					ImageID: iip.ImageID,
					//JobID:   iip.CommitJobID,
					//Status:  string(composeStatus.ImageStatus.Status),
					//Type:    "commit",
				},
			}

			// check commit if a job ID is set
			//if iip.CommitJobID != "" &&
			//	iip.CommitStatus != models.ImageStatusSuccess &&
			//	iip.CommitStatus != models.ImageStatusError {
			switch iip.CommitStatus {
			case models.ImageStatusBuilding, models.ImageStatusPending:
				// compare to status of compose in Image Builder
				var composeStatus *imagebuilder.ComposeStatus
				var err error

				composeStatus, err = ibClient.GetComposeStatus(iip.CommitJobID)
				if err != nil {
					log.Error("Error checking commit compose")
				}

				status := string(composeStatus.ImageStatus.Status)
				log.WithField("compose_status", status).Debug("Commit status from Image Builder")

				switch status {
				case composeStatusSuccess, composeStatusFailure:
					/*tx := db.DB.Save(&ibImage.Commit)
					if tx.Error != nil {
						log.WithField("error", tx.Error.Error()).Error("Error saving commit status to DB")
					} */

					// add installer specifics to the compose event
					edgePayload.EdgeComposeBasePayload.JobID = iip.CommitJobID
					edgePayload.EdgeComposeBasePayload.Status = status
					edgePayload.EdgeComposeBasePayload.Type = "commit"

					// create the edge event
					edgeEvent := kafkacommon.CreateEdgeEvent(iip.ImageOrgID, models.SourceEdgeEventAPI, iip.ImageRequestID,
						models.EventTypeEdgeCommitCompleted, iip.ImageName, edgePayload)

					// put the event on the bus
					if err = kafkacommon.ProduceEvent(kafkacommon.TopicFleetmgmtImageBuild, models.EventTypeEdgeCommitCompleted, edgeEvent); err != nil {
						log.WithField("request_id", edgeEvent.ID).Error("Producing the event failed")
					}
				}
				continue
			}

			// check installer if applicable and a job ID is set
			//if iip.InstallerStatus != models.ImageStatusNA &&
			//	iip.InstallerJobID != "" &&
			//	iip.InstallerStatus != models.ImageStatusSuccess &&
			//	iip.InstallerStatus != models.ImageStatusError {
			switch iip.InstallerStatus {
			case models.ImageStatusBuilding:
				// compare to status of compose in Image Builder
				var composeStatus *imagebuilder.ComposeStatus
				var err error

				composeStatus, err = ibClient.GetComposeStatus(iip.InstallerJobID)
				if err != nil {
					log.Error("Error checking commit compose")
				}

				status := string(composeStatus.ImageStatus.Status)
				log.WithField("compose_status", status).Debug("Commit status from Image Builder")

				switch status {
				case composeStatusSuccess, composeStatusFailure:
					/*tx := db.DB.Save(&ibImage.Commit)
					if tx.Error != nil {
						log.WithField("error", tx.Error.Error()).Error("Error saving commit status to DB")
					} */

					// add installer specifics to the compose event
					edgePayload.EdgeComposeBasePayload.JobID = iip.InstallerJobID
					edgePayload.EdgeComposeBasePayload.Status = status
					edgePayload.EdgeComposeBasePayload.Type = "installer"
					// create the edge event
					edgeEvent := kafkacommon.CreateEdgeEvent(iip.ImageOrgID, models.SourceEdgeEventAPI, iip.ImageRequestID,
						models.EventTypeEdgeInstallerCompleted, iip.ImageName, edgePayload)

					// put the event on the bus
					if err = kafkacommon.ProduceEvent(kafkacommon.TopicFleetmgmtImageBuild, models.EventTypeEdgeInstallerCompleted, edgeEvent); err != nil {
						log.WithField("request_id", edgeEvent.ID).Error("Producing the event failed")
					}
				}
			}
		}

		// moved to the end so it will start querying at startup
		time.Sleep(LoopTime)
	}
}

// replace this with a central identity func
func recreateIdentities(orgID string, account string) (identity.XRHID, string) {
	// recreate a stripped down identity header
	strippedIdentity := fmt.Sprintf(`{ "identity": {"account_number": "%s", "org_id": "%s", "type": "User", "internal": {"org_id": "%s"} } }`,
		account, orgID, orgID)
	//log.WithField("identity_text", strippedIdentity).Debug("Creating a new stripped identity")

	xrhidIdentity := &identity.XRHID{}
	err := json.Unmarshal([]byte(strippedIdentity), xrhidIdentity)
	if err != nil {
		log.WithField("error", err.Error()).Error("Error unmarshaling identity")
	}

	identityBytes, err := json.Marshal(xrhidIdentity)
	if err != nil {
		log.Error("Error marshaling the identity into a string")
	}
	base64Identity := base64.StdEncoding.EncodeToString(identityBytes)

	return *xrhidIdentity, base64Identity
}
