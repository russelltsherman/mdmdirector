package director

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"github.com/mdmdirector/mdmdirector/db"
	"github.com/mdmdirector/mdmdirector/log"
	"github.com/mdmdirector/mdmdirector/types"
	"github.com/mdmdirector/mdmdirector/utils"
	"github.com/pkg/errors"
)

const MAX = 5

var DevicesFetchedFromMDM bool

func getDelay() (time.Duration, time.Duration) {
	DelaySeconds := 7200

	if utils.DebugMode() {
		DelaySeconds = 20
	}

	HalfDelaySeconds := DelaySeconds / 2

	return time.Duration(DelaySeconds), time.Duration(HalfDelaySeconds)
}

func RetryCommands() {
	var delay time.Duration
	if utils.DebugMode() {
		delay = 20
	} else {
		delay = 120
	}
	ticker := time.NewTicker(delay * time.Second)
	defer ticker.Stop()
	fn := func() {
		pushNotNow()
	}

	fn()

	for range ticker.C {
		fn()
	}
}

func pushNotNow() error {
	var command types.Command
	var commands []types.Command
	err := db.DB.Model(&command).Select("DISTINCT(device_ud_id)").Where("status = ?", "NotNow").Scan(&commands).Error
	if err != nil {
		return err
	}

	client := &http.Client{}

	for _, queuedCommand := range commands {

		endpoint, err := url.Parse(utils.ServerURL())
		if err != nil {
			log.Error(err)
		}
		retry := time.Now().Unix() + 3600
		endpoint.Path = path.Join(endpoint.Path, "push", queuedCommand.DeviceUDID)
		log.Debug(endpoint.Path)
		queryString := endpoint.Query()
		queryString.Set("expiration", string(strconv.FormatInt(retry, 10)))
		endpoint.RawQuery = queryString.Encode()
		req, err := http.NewRequest("GET", endpoint.String(), nil)
		if err != nil {
			log.Error(err)
		}
		req.SetBasicAuth("micromdm", utils.APIKey())

		resp, err := client.Do(req)
		if err != nil {
			log.Error(err)
			continue
		}

		resp.Body.Close()
	}
	return nil
}

func shuffleDevices(vals []types.Device) []types.Device {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	ret := make([]types.Device, len(vals))
	perm := r.Perm(len(vals))
	for i, randIndex := range perm {
		ret[i] = vals[randIndex]
	}
	return ret
}

func pushAll() error {
	var devices []types.Device
	var dbDevices []types.Device
	now := time.Now()

	DelaySeconds, HalfDelaySeconds := getDelay()

	threeHoursAgo := time.Now().Add(-3 * time.Hour)
	lastCheckinDelay := time.Now().Add(-HalfDelaySeconds * time.Second)

	err := db.DB.Find(&dbDevices).Scan(&dbDevices).Error
	if err != nil {
		return err
	}

	for _, dbDevice := range dbDevices {

		// If it's been updated within the last three hours, try to push again as it might still be online
		if dbDevice.LastCheckedIn.After(threeHoursAgo) {
			log.Infof("%v checked in more than three hours ago", dbDevice.UDID)
			if now.Before(dbDevice.NextPush) {
				log.Infof("Not pushing to %v, next push is %v", dbDevice.UDID, dbDevice.NextPush)
				continue
			}
		}
		// This contrived bit of logic is to handle devices that don't have a LastScheduledPush set yet
		if !dbDevice.LastScheduledPush.Before(lastCheckinDelay) {
			log.Infof("%v last pushed in %v which is within %v seconds", dbDevice.UDID, dbDevice.LastScheduledPush, HalfDelaySeconds)
			continue
		}

		devices = append(devices, dbDevice)
	}

	log.Debug("Pushing to all in debug mode")
	sem := make(chan int, MAX)
	counter := 0
	total := 0
	devicesPerSecond := float64(len(devices)) / float64((DelaySeconds - 1))
	var shuffledDevices = shuffleDevices(devices)
	for i := range shuffledDevices {
		device := shuffledDevices[i]
		if float64(counter) >= devicesPerSecond {
			log.Infof("Sleeping due to having processed %v devices out of %v. Processing %v per 0.5 seconds.", total, len(devices), devicesPerSecond)
			time.Sleep(500 * time.Millisecond)
			counter = 0
		}
		log.Debug("Processed ", counter)
		sem <- 1 // will block if there is MAX ints in sem
		go func() {
			// pushConcurrent(device, client)
			err := AddDeviceToScheduledPushQueue(device)
			if err != nil {
				log.Error(err)
			}
			<-sem // removes an int from sem, allowing another to proceed
		}()
		counter++
		total++
	}
	log.Infof("Completed scheduling pushes to %v devices", len(devices))
	return nil
}

func AddDeviceToScheduledPushQueue(device types.Device) error {
	var scheduledPush types.ScheduledPush
	DelaySeconds, _ := getDelay()
	now := time.Now()
	var retry int64
	log.Infof("Adding scheduled push for %v", device.UDID)

	if now.After(device.NextPush) {
		log.Infof("After scheduled push of %v for %v. Pushing with an expiry of 24 hours", device.NextPush, device.UDID)
		retry = time.Now().Unix() + 86400
	} else {
		retry = time.Now().Unix() + int64(DelaySeconds)
	}

	err := db.DB.Model(&scheduledPush).FirstOrCreate(&scheduledPush, types.ScheduledPush{DeviceUDID: device.UDID, Expiration: retry}).Error
	if err != nil {
		return errors.Wrap(err, "AddDeviceToScheduledPushQueue::ScheduledPushFirstOrCreate")
	}

	return nil
}

func ProcessScheduledCheckinQueue() {

	ticker := time.NewTicker(1 * time.Second)
	client := &http.Client{}

	defer ticker.Stop()
	fn := func() {
		err := pushConcurrent(client)
		if err != nil {
			log.Error(err)
		}
	}

	fn()
	for range ticker.C {
		fn()
	}

}

func pushConcurrent(client *http.Client) error {

	var device types.Device
	var scheduledPush types.ScheduledPush
	var scheduledPushes []types.ScheduledPush
	now := time.Now()

	err := db.DB.Model(&scheduledPush).Where("status = ?", "pending").Limit(10).Scan(&scheduledPushes).Error
	if err != nil {
		return errors.Wrap(err, "pushConcurrent::retrievePendingPushes")
	}

	// Mark the devices we are woring on as "in_pogress" and then perform the push
	for _, push := range scheduledPushes {
		endpoint, err := url.Parse(utils.ServerURL())
		if err != nil {
			return errors.Wrap(err, "pushConcurrent::ParseServerURL")
		}
		err = db.DB.Model(&scheduledPush).Where("id = ?", push.ID).Update("status", "in_progress").Error
		if err != nil {
			log.Error(err)
			continue
		}

		log.Infof("Pushing to %v", push.DeviceUDID)

		endpoint.Path = path.Join(endpoint.Path, "push", push.DeviceUDID)
		queryString := endpoint.Query()
		queryString.Set("expiration", strconv.FormatInt(push.Expiration, 10))
		endpoint.RawQuery = queryString.Encode()
		req, err := http.NewRequest("GET", endpoint.String(), nil)
		if err != nil {
			log.Error(err)
			continue
		}
		req.SetBasicAuth("micromdm", utils.APIKey())

		resp, err := client.Do(req)
		if err != nil {
			log.Error(err)
			continue
		}

		err = db.DB.Delete(push).Error
		if err != nil {
			log.Error(err)
			continue
		}

		err = db.DB.Model(&device).Where("ud_id = ?", push.DeviceUDID).Updates(types.Device{
			LastScheduledPush: now,
			NextPush:          time.Now().Add(12 * time.Hour),
		}).Error
		if err != nil {
			log.Error(err)
			continue
		}

		resp.Body.Close()

	}
	return nil
}

func PushDevice(udid string) error {
	client := &http.Client{}

	endpoint, err := url.Parse(utils.ServerURL())
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}

	retry := time.Now().Unix() + 3600
	if utils.DebugMode() {
		retry = time.Now().Unix() + 30
	}
	endpoint.Path = path.Join(endpoint.Path, "push", udid)
	queryString := endpoint.Query()
	queryString.Set("expiration", string(strconv.FormatInt(retry, 10)))
	endpoint.RawQuery = queryString.Encode()
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}
	req.SetBasicAuth("micromdm", utils.APIKey())

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}

	err = resp.Body.Close()
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}

	return nil
}

func UnconfiguredDevices() {
	ticker := time.NewTicker(30 * time.Second)

	defer ticker.Stop()
	fn := func() {
		err := processUnconfiguredDevices()
		if err != nil {
			log.Error(err)
		}
	}

	fn()
	for range ticker.C {
		fn()
	}
}

func processUnconfiguredDevices() error {
	var awaitingConfigDevices []types.Device
	var awaitingConfigDevice types.Device

	err := db.DB.Model(&awaitingConfigDevice).Where("awaiting_configuration = ?", true).Scan(&awaitingConfigDevices).Error
	if err != nil {
		return err
	}

	for i := range awaitingConfigDevices {
		unconfiguredDevice := awaitingConfigDevices[i]
		log.Debugf("Running initial tasks due to schedule %v", unconfiguredDevice.UDID)
		err := RunInitialTasks(unconfiguredDevice.UDID)
		if err != nil {
			log.Error(err)
		}
	}

	return nil
}

func ScheduledCheckin() {
	// var delay time.Duration
	DelaySeconds, _ := getDelay()
	ticker := time.NewTicker(DelaySeconds * time.Second)
	if utils.DebugMode() {
		ticker = time.NewTicker(20 * time.Second)
	}

	for {
		if !DevicesFetchedFromMDM {
			time.Sleep(30 * time.Second)
			log.Info("Devices are still being fetched from MicroMDM")
		} else {
			break
		}
	}

	defer ticker.Stop()
	fn := func() {
		log.Infof("Running scheduled checkin (%v second) delay", DelaySeconds)
		err := processScheduledCheckin()
		if err != nil {
			log.Error(err)
		}
	}

	fn()

	for range ticker.C {
		go fn()
	}
}

func processScheduledCheckin() error {
	if utils.DebugMode() {
		log.Debug("Processing scheduledCheckin in debug mode")
	}

	err := pushAll()
	if err != nil {
		return err
	}

	var certificates []types.Certificate

	err = db.DB.Unscoped().Model(&certificates).Where("device_ud_id is NULL").Delete(&types.Certificate{}).Error
	if err != nil {
		return errors.Wrap(err, "processScheduledCheckin::CleanupNullCertificates")
	}

	var profileLists []types.ProfileList

	err = db.DB.Unscoped().Model(&profileLists).Where("device_ud_id is NULL").Delete(&types.ProfileList{}).Error
	if err != nil {
		return errors.Wrap(err, "processScheduledCheckin::CleanupNullProfileLists")
	}

	return nil
}

func FetchDevicesFromMDM() {
	var deviceModel types.Device
	var devices types.DevicesFromMDM
	log.Info("Fetching devices from MicroMDM...")

	// Handle Micro having a bad day
	var client = &http.Client{
		Timeout: time.Second * 60,
	}

	endpoint, err := url.Parse(utils.ServerURL())
	if err != nil {
		log.Error(err)
	}
	endpoint.Path = path.Join(endpoint.Path, "v1", "devices")

	req, _ := http.NewRequest("POST", endpoint.String(), bytes.NewBufferString("{}"))
	req.SetBasicAuth("micromdm", utils.APIKey())
	resp, err := client.Do(req)
	if err != nil {
		log.Error(err)
	}

	if resp.StatusCode != 200 {
		return
	}

	defer resp.Body.Close()

	responseData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
	}

	err = json.Unmarshal(responseData, &devices)
	if err != nil {
		log.Error(err)
	}

	for _, newDevice := range devices.Devices {
		var device types.Device
		device.UDID = newDevice.UDID
		device.SerialNumber = newDevice.SerialNumber
		device.Active = newDevice.EnrollmentStatus
		if newDevice.EnrollmentStatus {
			device.AuthenticateRecieved = true
			device.TokenUpdateRecieved = true
			device.InitialTasksRun = true
		}
		if newDevice.UDID == "" {
			continue
		}
		err := db.DB.Model(&deviceModel).Where("ud_id = ?", newDevice.UDID).FirstOrCreate(&device).Error
		if err != nil {
			log.Error(err)
		}

	}
	DevicesFetchedFromMDM = true
	log.Info("Finished fetching devices from MicroMDM...")
}
