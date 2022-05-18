package greenplum

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/blang/semver"
	"github.com/greenplum-db/gp-common-go-libs/cluster"
	"github.com/greenplum-db/gp-common-go-libs/gplog"
	"github.com/jackc/pgx"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/internal/databases/postgres"
	"github.com/wal-g/wal-g/utility"
)

const (
	BackupNamePrefix   = "backup_"
	BackupNameLength   = 23 // len(BackupNamePrefix) + len(utility.BackupTimeFormat)
	SegBackupLogPrefix = "wal-g-log"
)

// BackupArguments holds all arguments parsed from cmd to this handler class
type BackupArguments struct {
	isPermanent    bool
	userData       interface{}
	segmentFwdArgs []SegmentFwdArg
	logsDir        string

	segPollInterval time.Duration
	segPollRetries  int
}

type SegmentUserData struct {
	ID string `json:"id"`
}

func NewSegmentUserData() SegmentUserData {
	return SegmentUserData{ID: uuid.New().String()}
}

func NewSegmentUserDataFromID(backupID string) SegmentUserData {
	return SegmentUserData{ID: backupID}
}

func (d SegmentUserData) String() string {
	b, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// QuotedString will do json.Marshal-ing followed by quoting in order to escape special control characters
// in the resulting JSON so it can be transferred as the cmdline argument to a segment
func (d SegmentUserData) QuotedString() string {
	return strconv.Quote(d.String())
}

// SegmentFwdArg describes the specific WAL-G
// arguments that is going to be forwarded to the segments
type SegmentFwdArg struct {
	Name  string
	Value string
}

// BackupWorkers holds the external objects that the handler uses to get the backup data / write the backup data
type BackupWorkers struct {
	Uploader *internal.Uploader
	Conn     *pgx.Conn
}

// CurrBackupInfo holds all information that is harvest during the backup process
type CurrBackupInfo struct {
	backupName           string
	segmentBackups       map[string]*cluster.SegConfig
	startTime            time.Time
	systemIdentifier     *uint64
	gpVersion            semver.Version
	segmentsMetadata     map[string]PgSegmentMetaDto
	backupPidByContentID map[int]int
}

// BackupHandler is the main struct which is handling the backup process
type BackupHandler struct {
	arguments      BackupArguments
	workers        BackupWorkers
	globalCluster  *cluster.Cluster
	currBackupInfo CurrBackupInfo
}

// TODO: unit tests
// buildBackupPushCommand builds a command to be executed on specific segment
func (bh *BackupHandler) buildBackupPushCommand(contentID int) string {
	segment := bh.globalCluster.ByContent[contentID][0]
	segUserData := NewSegmentUserData()
	bh.currBackupInfo.segmentBackups[segUserData.ID] = segment

	backupPushArgs := []string{
		segment.DataDir,
		fmt.Sprintf("--add-user-data=%s", segUserData.String()),
		fmt.Sprintf("--pgport=%d", segment.Port),
	}

	for _, arg := range bh.arguments.segmentFwdArgs {
		backupPushArgs = append(backupPushArgs, fmt.Sprintf("--%s=%s", arg.Name, arg.Value))
	}

	backupPushArgsLine := "'" + strings.Join(backupPushArgs, " ") + "'"

	cmd := []string{
		// nohup to avoid the SIGHUP on SSH session disconnect
		"nohup", "wal-g seg-backup-push",
		fmt.Sprintf("--content-id=%d", segment.ContentID),
		// name of the current backup to format the state file name
		bh.currBackupInfo.backupName,
		// actual arguments to be passed to the backup-push command
		backupPushArgsLine,
		// pass the config file location
		fmt.Sprintf("--config=%s", internal.CfgFile),
		// forward stdout and stderr to the log file
		"&>>", formatSegmentLogPath(contentID),
		// run in the background and get the launched process PID
		"& echo $!",
	}

	cmdLine := strings.Join(cmd, " ")
	tracelog.InfoLogger.Printf("Command to run on segment %d: %s", contentID, cmdLine)
	return cmdLine
}

// HandleBackupPush handles the backup being read from filesystem and being pushed to the repository
func (bh *BackupHandler) HandleBackupPush() {
	bh.currBackupInfo.backupName = BackupNamePrefix + time.Now().Format(utility.BackupTimeFormat)
	bh.currBackupInfo.startTime = utility.TimeNowCrossPlatformUTC()
	initGpLog(bh.arguments.logsDir)

	err := bh.checkPrerequisites()
	tracelog.ErrorLogger.FatalfOnError("Backup prerequisites check failed: %v\n", err)

	tracelog.InfoLogger.Println("Running wal-g on segments")
	remoteOutput := bh.globalCluster.GenerateAndExecuteCommand("Running wal-g",
		cluster.ON_SEGMENTS|cluster.INCLUDE_MASTER,
		func(contentID int) string {
			return bh.buildBackupPushCommand(contentID)
		})
	bh.globalCluster.CheckClusterError(remoteOutput, "Unable to run wal-g", func(contentID int) string {
		return "Unable to run wal-g"
	}, true)

	for _, command := range remoteOutput.Commands {
		if command.Stderr != "" {
			tracelog.ErrorLogger.Printf("stderr (segment %d):\n%s\n", command.Content, command.Stderr)
		}
	}

	bh.currBackupInfo.backupPidByContentID, err = extractBackupPids(remoteOutput)
	// this is a non-critical error since backup PIDs are only useful if backup is aborted
	tracelog.ErrorLogger.PrintOnError(err)
	if remoteOutput.NumErrors > 0 {
		bh.abortBackup()
	}

	// WAL-G will reconnect later
	bh.disconnect()

	// wait for segments to complete their backups
	waitBackupsErr := bh.waitSegmentBackups()
	if waitBackupsErr != nil {
		tracelog.ErrorLogger.Printf("Segment backups wait error: %v", waitBackupsErr)
	}
	tracelog.ErrorLogger.FatalfOnError("Failed to connect to the greenplum master: %v",
		bh.connect())
	if waitBackupsErr != nil {
		bh.abortBackup()
	}

	restoreLSNs, err := createRestorePoint(bh.workers.Conn, bh.currBackupInfo.backupName)
	tracelog.ErrorLogger.FatalOnError(err)

	bh.currBackupInfo.segmentsMetadata, err = bh.fetchSegmentBackupsMetadata()
	tracelog.ErrorLogger.FatalOnError(err)

	sentinelDto := NewBackupSentinelDto(bh.currBackupInfo, restoreLSNs, bh.arguments.userData, bh.arguments.isPermanent)
	err = bh.uploadSentinel(sentinelDto)
	if err != nil {
		tracelog.ErrorLogger.Printf("Failed to upload sentinel file for backup: %s", bh.currBackupInfo.backupName)
		tracelog.ErrorLogger.FatalError(err)
	}
	tracelog.InfoLogger.Printf("Backup %s successfully created", bh.currBackupInfo.backupName)
	bh.disconnect()
}

func (bh *BackupHandler) waitSegmentBackups() error {
	ticker := time.NewTicker(bh.arguments.segPollInterval)
	retryCount := bh.arguments.segPollRetries
	for {
		<-ticker.C
		states, err := bh.pollSegmentStates()
		if err != nil {
			if retryCount == 0 {
				return fmt.Errorf("gave up polling the backup-push states (tried %d times): %v", bh.arguments.segPollRetries, err)
			}
			retryCount--
			tracelog.WarningLogger.Printf("failed to poll segment backup-push states, will try again %d more times", retryCount)
			continue
		}
		// reset retries after the successful poll
		retryCount = bh.arguments.segPollRetries

		runningBackups, err := checkBackupStates(states)
		if err != nil {
			return err
		}

		if runningBackups == 0 {
			tracelog.InfoLogger.Printf("No running backups left.")
			return nil
		}
	}
}

// TODO: unit tests
func checkBackupStates(states map[int]SegBackupState) (int, error) {
	runningBackupsCount := 0

	tracelog.InfoLogger.Printf("backup-push states:")
	for contentID, state := range states {
		tracelog.InfoLogger.Printf("content ID: %d, status: %s, ts: %s", contentID, state.Status, state.TS)
	}

	for contentID, state := range states {
		switch state.Status {
		case RunningBackupStatus:
			// give up if the heartbeat ts is too old
			if state.TS.Add(15 * time.Minute).Before(time.Now()) {
				return 0, fmt.Errorf("giving up waiting for segment %d: last seen on %s", contentID, state.TS)
			}
			runningBackupsCount++

		case FailedBackupStatus, InterruptedBackupStatus:
			return 0, fmt.Errorf("unexpected backup status: %s on segment %d at %s", state.Status, contentID, state.TS)
		}
	}

	return runningBackupsCount, nil
}

func extractBackupPids(output *cluster.RemoteOutput) (map[int]int, error) {
	backupPids := make(map[int]int)
	var resErr error

	for _, command := range output.Commands {
		pid, err := strconv.Atoi(strings.TrimSpace(command.Stdout))
		if err != nil {
			resErr = fmt.Errorf("%w; failed to parse the backup PID: %v", resErr, err)
			continue
		}

		backupPids[command.Content] = pid
	}

	tracelog.InfoLogger.Printf("WAL-G segment PIDs: %v", backupPids)
	return backupPids, resErr
}

func (bh *BackupHandler) pollSegmentStates() (map[int]SegBackupState, error) {
	segmentStates := make(map[int]SegBackupState)
	remoteOutput := bh.globalCluster.GenerateAndExecuteCommand("Polling the segment backup-push statuses...",
		cluster.ON_SEGMENTS|cluster.EXCLUDE_MIRRORS|cluster.INCLUDE_MASTER,
		func(contentID int) string {
			cmd := fmt.Sprintf("cat %s", FormatBackupStatePath(contentID, bh.currBackupInfo.backupName))
			tracelog.DebugLogger.Printf("Command to run on segment %d: %s", contentID, cmd)
			return cmd
		})

	bh.globalCluster.CheckClusterError(remoteOutput, "Unable to poll segment backup-push states", func(contentID int) string {
		return fmt.Sprintf("Unable to poll backup-push state on segment %d", contentID)
	}, true)

	for _, command := range remoteOutput.Commands {
		logger := tracelog.DebugLogger
		if command.Stderr != "" {
			logger = tracelog.WarningLogger
		}
		logger.Printf("Poll segment backup-push state STDERR (segment %d):\n%s\n", command.Content, command.Stderr)
		logger.Printf("Poll segment backup-push state STDOUT (segment %d):\n%s\n", command.Content, command.Stdout)
	}

	if remoteOutput.NumErrors > 0 {
		return nil, fmt.Errorf("encountered one or more errors during the polling. See %s for a complete list of errors",
			gplog.GetLogFilePath())
	}

	for _, command := range remoteOutput.Commands {
		backupState := SegBackupState{}
		err := json.Unmarshal([]byte(command.Stdout), &backupState)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal state JSON file: %v", err)
		}
		segmentStates[command.Content] = backupState
	}

	return segmentStates, nil
}

func (bh *BackupHandler) checkPrerequisites() (err error) {
	tracelog.InfoLogger.Println("Checking backup prerequisites")

	if bh.currBackupInfo.gpVersion.Major >= 7 {
		// GP7+ allows the non-exclusive backups
		tracelog.InfoLogger.Println("Checking backup prerequisites: OK")
		return nil
	}

	tracelog.InfoLogger.Println("Checking for the existing running backup...")
	queryRunner, err := NewGpQueryRunner(bh.workers.Conn)
	if err != nil {
		return err
	}
	backupStatuses, err := queryRunner.IsInBackup()
	if err != nil {
		return err
	}

	isInBackupSegments := make([]int, 0)
	for contentID, isInBackup := range backupStatuses {
		if isInBackup {
			isInBackupSegments = append(isInBackupSegments, contentID)
		}
	}

	if len(isInBackupSegments) > 0 {
		return fmt.Errorf("backup is already in progress on one or more segments: %v", isInBackupSegments)
	}
	tracelog.InfoLogger.Printf("No running backups were found")
	tracelog.InfoLogger.Printf("Checking backup prerequisites: OK")
	return nil
}

// nolint:gocritic
func (bh *BackupHandler) uploadSentinel(sentinelDto BackupSentinelDto) (err error) {
	tracelog.InfoLogger.Println("Uploading sentinel file")
	tracelog.InfoLogger.Println(sentinelDto.String())

	sentinelUploader := bh.workers.Uploader
	sentinelUploader.UploadingFolder = sentinelUploader.UploadingFolder.GetSubFolder(utility.BaseBackupPath)
	return internal.UploadSentinel(sentinelUploader, sentinelDto, bh.currBackupInfo.backupName)
}

func (bh *BackupHandler) connect() (err error) {
	tracelog.InfoLogger.Println("Connecting to Greenplum master.")
	bh.workers.Conn, err = postgres.Connect()
	return err
}

func (bh *BackupHandler) disconnect() {
	tracelog.InfoLogger.Println("Disconnecting from the Greenplum master.")
	err := bh.workers.Conn.Close()
	if err != nil {
		tracelog.WarningLogger.Printf("Failed to disconnect: %v", err)
	}
}

func getGpClusterInfo(conn *pgx.Conn) (globalCluster *cluster.Cluster, version semver.Version, systemIdentifier *uint64, err error) {
	queryRunner, err := NewGpQueryRunner(conn)
	if err != nil {
		return globalCluster, semver.Version{}, nil, err
	}

	versionStr, err := queryRunner.GetGreenplumVersion()
	if err != nil {
		return globalCluster, semver.Version{}, nil, err
	}
	tracelog.InfoLogger.Printf("Greenplum version: %s", versionStr)
	versionStart := strings.Index(versionStr, "(Greenplum Database ") + len("(Greenplum Database ")
	versionEnd := strings.Index(versionStr, ")")
	versionStr = versionStr[versionStart:versionEnd]
	pattern := regexp.MustCompile(`\d+\.\d+\.\d+`)
	threeDigitVersion := pattern.FindStringSubmatch(versionStr)[0]
	semVer, err := semver.Make(threeDigitVersion)
	if err != nil {
		return globalCluster, semver.Version{}, nil, err
	}

	segConfigs, err := queryRunner.GetGreenplumSegmentsInfo(semVer)
	if err != nil {
		return globalCluster, semver.Version{}, nil, err
	}
	globalCluster = cluster.NewCluster(segConfigs)

	return globalCluster, semVer, queryRunner.pgQueryRunner.SystemIdentifier, nil
}

// NewBackupHandler returns a backup handler object, which can handle the backup
func NewBackupHandler(arguments BackupArguments) (bh *BackupHandler, err error) {
	uploader, err := internal.ConfigureUploader()
	if err != nil {
		return nil, err
	}

	conn, err := postgres.Connect()
	if err != nil {
		return nil, err
	}

	globalCluster, version, systemIdentifier, err := getGpClusterInfo(conn)
	if err != nil {
		return nil, err
	}

	bh = &BackupHandler{
		arguments: arguments,
		workers: BackupWorkers{
			Uploader: uploader,
			Conn:     conn,
		},
		globalCluster: globalCluster,
		currBackupInfo: CurrBackupInfo{
			segmentBackups:   make(map[string]*cluster.SegConfig),
			gpVersion:        version,
			systemIdentifier: systemIdentifier,
		},
	}
	return bh, nil
}

// NewBackupArguments creates a BackupArgument object to hold the arguments from the cmd
func NewBackupArguments(isPermanent bool, userData interface{}, fwdArgs []SegmentFwdArg, logsDir string,
	segPollInterval time.Duration, segPollRetries int) BackupArguments {
	return BackupArguments{
		isPermanent:     isPermanent,
		userData:        userData,
		segmentFwdArgs:  fwdArgs,
		logsDir:         logsDir,
		segPollInterval: segPollInterval,
		segPollRetries:  segPollRetries,
	}
}

func (bh *BackupHandler) fetchSegmentBackupsMetadata() (map[string]PgSegmentMetaDto, error) {
	metadata := make(map[string]PgSegmentMetaDto)

	backupIDs := make([]string, 0)
	for backupID := range bh.currBackupInfo.segmentBackups {
		backupIDs = append(backupIDs, backupID)
	}

	i := 0
	minFetchMetaRetryWait := 5 * time.Second
	maxFetchMetaRetryWait := time.Minute
	sleeper := internal.NewExponentialSleeper(minFetchMetaRetryWait, maxFetchMetaRetryWait)
	retryCount := 0
	maxRetryCount := 5

	for i < len(backupIDs) {
		meta, err := bh.fetchSingleMetadata(backupIDs[i], bh.currBackupInfo.segmentBackups[backupIDs[i]])
		if err != nil {
			// Due to the potentially large number of segments, a large number of ListObjects() requests can be produced instantly.
			// Instead of failing immediately, sleep and retry a couple of times.
			if retryCount < maxRetryCount {
				retryCount++
				sleeper.Sleep()
				continue
			}

			return nil, fmt.Errorf("failed to download the segment backup %s metadata (tried %d times): %w",
				backupIDs[i], retryCount, err)
		}
		metadata[backupIDs[i]] = *meta
		retryCount = 0
		i++
	}

	return metadata, nil
}

func (bh *BackupHandler) fetchSingleMetadata(backupID string, segCfg *cluster.SegConfig) (*PgSegmentMetaDto, error) {
	// Actually, this is not a real completed backup. It is only used to fetch the segment metadata
	currentBackup := NewBackup(bh.workers.Uploader.UploadingFolder, bh.currBackupInfo.backupName)

	pgBackup, err := currentBackup.GetSegmentBackup(backupID, segCfg.ContentID)
	if err != nil {
		return nil, err
	}

	meta := PgSegmentMetaDto{
		BackupName: pgBackup.Name,
	}

	meta.ExtendedMetadataDto, err = pgBackup.FetchMeta()
	if err != nil {
		return nil, err
	}

	return &meta, nil
}

func (bh *BackupHandler) abortBackup() {
	err := bh.terminateRunningBackups()
	if err != nil {
		tracelog.WarningLogger.Printf("Failed to stop running backups: %v", err)
	}

	err = bh.terminateWalgProcesses()
	if err != nil {
		tracelog.WarningLogger.Printf("Failed to shutdown the running WAL-G processes: %v", err)
	}

	tracelog.InfoLogger.Fatalf("Encountered one or more errors during the backup-push. See %s for a complete list of errors.",
		gplog.GetLogFilePath())
}

func (bh *BackupHandler) terminateRunningBackups() error {
	// Abort the non-finished exclusive backups on the segments.
	// WAL-G in GP7+ uses the non-exclusive backups, that are terminated on connection close, so this is unnecessary.
	if bh.currBackupInfo.gpVersion.Major >= 7 {
		return nil
	}

	tracelog.InfoLogger.Println("Terminating the running exclusive backups...")
	queryRunner, err := NewGpQueryRunner(bh.workers.Conn)
	if err != nil {
		return err
	}
	return queryRunner.AbortBackup()
}

func (bh *BackupHandler) terminateWalgProcesses() error {
	knownPidsLen := len(bh.currBackupInfo.backupPidByContentID)
	if knownPidsLen == 0 {
		return fmt.Errorf("there are no known PIDs of WAL-G segment processess")
	}

	if knownPidsLen < len(bh.globalCluster.ContentIDs) {
		tracelog.WarningLogger.Printf("Known PIDs count (%d) is less than the total segments number (%d)",
			knownPidsLen, len(bh.globalCluster.ContentIDs))
	}

	remoteOutput := bh.globalCluster.GenerateAndExecuteCommand("Terminating the segment backup-push processes...",
		cluster.ON_SEGMENTS|cluster.EXCLUDE_MIRRORS|cluster.INCLUDE_MASTER,
		func(contentID int) string {
			backupPid, ok := bh.currBackupInfo.backupPidByContentID[contentID]
			if !ok {
				return ""
			}

			return fmt.Sprintf("kill %d", backupPid)
		})

	bh.globalCluster.CheckClusterError(remoteOutput, "Unable to terminate backup-push processes", func(contentID int) string {
		return fmt.Sprintf("Unable to terminate backup-push process on segment %d", contentID)
	}, true)

	for _, command := range remoteOutput.Commands {
		if command.Stderr == "" {
			continue
		}

		tracelog.WarningLogger.Printf("Unable to terminate backup-push process (segment %d):\n%s\n", command.Content, command.Stderr)
	}

	return nil
}
