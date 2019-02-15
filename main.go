package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/sts"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	exitCodeOk             int = 0
	exitCodeError          int = 1
	exitCodeDockerError    int = 2
	exitCodeFlagParseError     = 10 + iota
	exitCodeAWSError
)

var (
	cfgFile string
	profile string
	region  string
	taskdef string
	action  string
)

var log = logrus.New()

func main() {
	var rootCmd = &cobra.Command{
		Use:     "ecs-local [flags] -t task_def -a 'command...'",
		Args:    cobra.ArbitraryArgs,
		Version: "v0.2.3",
		Run:     run,
		Example: "ecs-local -t stage-accounts -m src:dest -c ecs-local-config.yaml -a 'bundle exec rails c'",
	}
	// define Cobra Persistent Flags
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "read from or write (-w) to config file (default is ecs-local-config.yaml)")
	rootCmd.PersistentFlags().StringVarP(&profile, "profile", "p", "", "AWS profile")
	rootCmd.PersistentFlags().StringVarP(&region, "region", "r", "", "AWS region")
	rootCmd.PersistentFlags().StringVarP(&taskdef, "taskdef", "t", "", "task definition")
	// rootCmd.MarkPersistentFlagRequired("taskdef")
	rootCmd.PersistentFlags().StringVarP(&action, "action", "a", "", "commands/actions to be executed")
	rootCmd.PersistentFlags().StringSliceP("mounts", "m", []string{}, "mounts src:dest")
	rootCmd.PersistentFlags().StringSliceP("envs", "e", []string{}, "Env variables key=value")
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().BoolP("write", "w", false, "write to config file in -c and exit")

	// Bind Persistent Flags to Viper config
	viper.BindPFlags(rootCmd.PersistentFlags())

	// Set reasonable defaults for Viper config
	viper.SetDefault("config", "ecs-local-config.yaml")
	viper.SetDefault("profile", "default")
	viper.SetDefault("region", "us-east-1")
	viper.SetDefault("action", "bundle exec rails c")

	if err := rootCmd.Execute(); err != nil {
		log.Debugf("\n%+v\n", err)
		os.Exit(exitCodeError)
	}
}

func run(cmd *cobra.Command, args []string) {
	// Setup Logging level
	log.SetLevel(logrus.ErrorLevel)
	if viper.GetBool("verbose") == true {
		log.SetLevel(logrus.DebugLevel)
	}

	// Write flags to a config file
	if viper.GetBool("write") == true {

		// Get the current working dir
		dir, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}

		// overwrite the "write" bool when saving the config
		viper.Set("write", false)

		// Write to the passed config file if passed otherwise write to the default
		if cfgFile != "" {
			os.OpenFile(fmt.Sprintf("%s/%s", dir, cfgFile), os.O_RDONLY|os.O_CREATE, 0666)
			viper.SetConfigFile(cfgFile)
		} else {
			os.OpenFile(fmt.Sprintf("%s/ecs-local-config.yaml", dir), os.O_RDONLY|os.O_CREATE, 0666)
			viper.SetConfigName("ecs-local-config") // name of config file (without extension)
		}
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
		viper.WriteConfig()
		log.Println("Config saved")
		os.Exit(exitCodeOk)
	}

	// Reading flags from a config file
	if (viper.GetBool("write") == false) && (cfgFile != "") {

		// get the filepath
		abs, err := filepath.Abs(cfgFile)
		if err != nil {
			log.Debugf("Error reading filepath: %s", err.Error())
		}

		// get the config name
		base := filepath.Base(abs)

		// get the paths
		path := filepath.Dir(abs)

		//
		viper.SetConfigName(strings.Split(base, ".")[0])
		viper.AddConfigPath(path)

		// Find and read the config file; Handle errors reading the config file
		if err := viper.ReadInConfig(); err != nil {
			log.Debugf("Failed to read config file: %s", err.Error())
			os.Exit(exitCodeError)
		}
	}

	// TODO need to build a function for the reading in of configs sometime.
	// Reading flags from default file if it exists
	if (viper.GetBool("write") == false) && (cfgFile == "") {
		defaultConfigFile := "ecs-local-config.yaml"

		if _, err := os.Stat(defaultConfigFile); os.IsNotExist(err) {
			log.Debugf("No default config found: %s", err.Error())
		} else {
			log.Debugf("default config found parsing for flags")

			// get the filepath
			abs, err := filepath.Abs(defaultConfigFile)
			if err != nil {
				log.Debugf("Error reading filepath: %s", err.Error())
			}

			// get the config name
			base := filepath.Base(abs)

			// get the paths
			path := filepath.Dir(abs)

			//
			viper.SetConfigName(strings.Split(base, ".")[0])
			viper.AddConfigPath(path)

			// Find and read the config file; Handle errors reading the config file
			if err := viper.ReadInConfig(); err != nil {
				log.Debugf("Failed to read config file: %s", err.Error())
				os.Exit(exitCodeError)
			}
		}

	}

	if viper.GetString("taskdef") == "" {
		fmt.Println("no taskdef defined")
		cmd.Help()
		os.Exit(exitCodeOk)
	}

	taskDefinitionName := viper.GetString("taskdef")

	// set desired AWS region
	awsRegion := viper.GetString("region")
	if envRegion, present := os.LookupEnv("AWS_REGION"); present {
		awsRegion = envRegion
		log.Debugf("Using AWS_REGION from ENV")
	}

	// set desired AWS profile
	awsProfile := viper.GetString("profile")
	if envProfile, present := os.LookupEnv("AWS_PROFILE"); present {
		awsProfile = envProfile
		log.Debugf("Using AWS_PROFILE from ENV")
	}

	log.Debugf("Using AWS region \"%s\" ", awsRegion)
	log.Debugf("Using AWS profile \"%s\" ", awsProfile)

	// override default sts session duration
	stscreds.DefaultDuration = time.Duration(1) * time.Hour

	sess := session.Must(session.NewSessionWithOptions(session.Options{
		AssumeRoleTokenProvider: stscreds.StdinTokenProvider,
		SharedConfigState:       session.SharedConfigEnable,
		Profile:                 awsProfile,
		Config:                  aws.Config{Region: aws.String(awsRegion)},
	}))

	sess.Config.Credentials = credentials.NewCredentials(&CredentialCacheProvider{
		Creds:   sess.Config.Credentials,
		Profile: awsProfile,
	})

	svc := ecs.New(sess)
	resp, err := svc.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefinitionName),
	})

	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeAWSError)
	}

	if log.Level == logrus.DebugLevel {
		creds, _ := sess.Config.Credentials.Get()
		log.Debugf("Credential provider is %s", creds.ProviderName)
	}

	task := resp.TaskDefinition
	image := task.ContainerDefinitions[0].Image

	log.Debugf("Found task %s", *task.TaskDefinitionArn)

	ecrClient := ecr.New(sess)
	input := &ecr.GetAuthorizationTokenInput{}
	result, err := ecrClient.GetAuthorizationToken(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	authData := result.AuthorizationData[0]
	token := authData.AuthorizationToken

	data, err := base64.StdEncoding.DecodeString(*token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	userpass := strings.Split(string(data), ":")
	if len(userpass) != 2 {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	auth := docker.AuthConfiguration{
		Username:      userpass[0],
		Password:      userpass[1],
		ServerAddress: *authData.ProxyEndpoint,
	}

	endpoint := "unix:///var/run/docker.sock"
	client, err := docker.NewClient(endpoint)

	pullOptions := docker.PullImageOptions{
		Repository: *image,
	}

	fmt.Printf("Pulling %s \n", *image)
	pullOptions.OutputStream = os.Stdout

	err = client.PullImage(pullOptions, auth)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	dockerArgs := []string{"run", "-it", "--rm"}

	// set docker command
	command := strings.Split(viper.GetString("action"), " ")
	if len(command) == 0 {
		for _, v := range task.ContainerDefinitions[0].Command {
			command = append(command, *v)
		}
	}
	log.Debugf("Running command \"%s\"", strings.Join(command, " "))

	// envs
	for _, e := range task.ContainerDefinitions[0].Environment {
		dockerArgs = append(dockerArgs, "-e", fmt.Sprintf("%s=%s", *e.Name, *e.Value))
	}

	// attempt to assume container IAM role
	if task.TaskRoleArn != nil {
		stsClient := sts.New(sess)
		role, err := stsClient.AssumeRole(&sts.AssumeRoleInput{
			DurationSeconds: aws.Int64(3600),
			RoleArn:         task.TaskRoleArn,
			RoleSessionName: aws.String("ecs-local"),
		})
		if err != nil {
			log.Debugf("Unable to assume role %s", *task.TaskRoleArn)
			log.Debugf("%s", err.Error())
		} else {
			log.Debugf("Successfully assumed container role %s", *task.TaskRoleArn)
			dockerArgs = append(dockerArgs,
				"-e", fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", *role.Credentials.AccessKeyId),
				"-e", fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", *role.Credentials.SecretAccessKey),
				"-e", fmt.Sprintf("AWS_SESSION_TOKEN=%s", *role.Credentials.SessionToken),
			)
		}
	}

	// parse mounts flags
	mounts := viper.GetStringSlice("mounts")
	if len(mounts) > 0 {
		for _, mount := range mounts {
			parts := strings.SplitN(mount, ":", 2)
			dockerArgs = append(dockerArgs,
				"-v", fmt.Sprintf("%s:%s", parts[0], parts[1]))
		}
	}

	// parse environment flags
	envs := viper.GetStringSlice("envs")
	if len(envs) > 0 {
		for _, env := range envs {
			parts := strings.SplitN(env, "=", 2)
			dockerArgs = append(dockerArgs,
				"-e", fmt.Sprintf("%s=%s", parts[0], parts[1]))
		}
	}

	dockerArgs = append(dockerArgs, *image)

	// start the container
	dockerCmd := exec.Command("docker", append(dockerArgs, command...)...)
	dockerCmd.Stdin = os.Stdin
	dockerCmd.Stdout = os.Stdout
	dockerCmd.Stderr = os.Stderr

	dockerCmd.Start()
	dockerCmd.Wait()
}
