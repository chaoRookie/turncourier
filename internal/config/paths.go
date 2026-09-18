// Package config 解析配置文件与数据目录位置；环境变量给出的路径必须是绝对路径。
package config

import (
	"errors"
	"fmt"
	"path/filepath"
)

const (
	// configEnv 指定配置文件的绝对路径。
	configEnv = "TURNCOURIER_CONFIG"
	// dataDirEnv 指定数据目录的绝对路径。
	dataDirEnv = "TURNCOURIER_DATA_DIR"
	// appDirName 是用户配置目录下的应用目录名。
	appDirName = "TurnCourier"
	// configFileName 是默认的配置文件名。
	configFileName = "turncourier.toml"
	// databaseFileName 是数据目录中的数据库文件名。
	databaseFileName = "turncourier.db"
)

// Paths 是解析后的配置文件、数据目录与数据库文件绝对路径。
type Paths struct {
	ConfigFile string
	DataDir    string
	Database   string
}

// ResolvePaths 根据环境变量与用户配置目录计算路径；环境变量给出的路径必须是绝对路径。
// 只有在未设置 TURNCOURIER_CONFIG 时才会调用 userConfigDir，错误文本不包含环境变量的取值。
func ResolvePaths(getenv func(string) string, userConfigDir func() (string, error)) (Paths, error) {
	configFile := getenv(configEnv)
	if configFile != "" {
		if !filepath.IsAbs(configFile) {
			return Paths{}, errors.New(configEnv + " must be an absolute path")
		}
		configFile = filepath.Clean(configFile)
	} else {
		dir, err := userConfigDir()
		if err != nil {
			return Paths{}, fmt.Errorf("cannot determine the user config directory: %w", err)
		}
		configFile = filepath.Join(dir, appDirName, configFileName)
	}
	dataDir := getenv(dataDirEnv)
	if dataDir != "" {
		if !filepath.IsAbs(dataDir) {
			return Paths{}, errors.New(dataDirEnv + " must be an absolute path")
		}
		dataDir = filepath.Clean(dataDir)
	} else {
		dataDir = filepath.Dir(configFile)
	}
	return Paths{
		ConfigFile: configFile,
		DataDir:    dataDir,
		Database:   filepath.Join(dataDir, databaseFileName),
	}, nil
}
