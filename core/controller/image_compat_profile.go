package controller

import "github.com/labring/aiproxy/core/model"

const ImageCompatConfigKey = model.OpenAIImageProfileKey

type imageCompatProfile = model.OpenAIImageProfile

func validImageCompatProfile(mc model.ModelConfig) bool { return model.ValidOpenAIImageProfile(mc) }

func selectImageCompatProfile(
	configs map[string]model.ModelConfig,
	requested string,
) (model.ModelConfig, bool) {
	return model.SelectOpenAIImageProfile(configs, requested)
}
