package api

type PlayerResponse struct {
	ActionRange        int                   `json:"actionRange"`
	Beavers            []PlayerBeaver        `json:"beavers"`
	Cells              []TerraformedCell     `json:"cells"`
	Construction       []Construction        `json:"construction"`
	Enemy              []EnemyPlantation     `json:"enemy"`
	MeteoForecasts     []MeteoForecast       `json:"meteoForecasts"`
	Mountains          [][]int               `json:"mountains"`
	NextTurnIn         float64               `json:"nextTurnIn"`
	PlantationUpgrades PlantationUpgrades    `json:"plantationUpgrades"`
	Plantations        []Plantation          `json:"plantations"`
	Size               []int                 `json:"size"`
	TurnNo             int                   `json:"turnNo"`
}

type PlayerBeaver struct {
	Hp       int    `json:"hp"`
	Id       string `json:"id"`
	Position []int  `json:"position"`
}

type TerraformedCell struct {
	Position               []int `json:"position"`
	TerraformationProgress int   `json:"terraformationProgress"`
	TurnsUntilDegradation  int   `json:"turnsUntilDegradation"`
}

type Construction struct {
	Position []int `json:"position"`
	Progress int   `json:"progress"`
}

type EnemyPlantation struct {
	Hp       int    `json:"hp"`
	Id       string `json:"id"`
	Position []int  `json:"position"`
}

type MeteoForecast struct {
	Forming      bool   `json:"forming"`
	Id           string `json:"id"`
	Kind         string `json:"kind"`
	NextPosition []int  `json:"nextPosition,omitempty"`
	Position     []int  `json:"position"`
	Radius       int    `json:"radius"`
	TurnsUntil   int    `json:"turnsUntil"`
}

type PlantationUpgrades struct {
	IntervalTurns    int                        `json:"intervalTurns"`
	MaxPoints        int                        `json:"maxPoints"`
	Points           int                        `json:"points"`
	Tiers            []PlantationUpgradeTierItem `json:"tiers"`
	TurnsUntilPoints int                        `json:"turnsUntilPoints"`
}

type PlantationUpgradeTierItem struct {
	Current int    `json:"current"`
	Max     int    `json:"max"`
	Name    string `json:"name"`
}

type Plantation struct {
	Hp                int    `json:"hp"`
	Id                string `json:"id"`
	ImmunityUntilTurn int    `json:"immunityUntilTurn"`
	IsIsolated        bool   `json:"isIsolated"`
	IsMain            bool   `json:"isMain"`
	Position          []int  `json:"position"`
}

type PlayerCommand struct {
	Command           []PlantationAction  `json:"command,omitempty"`
	PlantationUpgrade string              `json:"plantationUpgrade,omitempty"`
	RelocateMain      []int               `json:"relocateMain,omitempty"`
}

type PlantationAction struct {
	Path [][]int `json:"path"`
}

type LogMessage struct {
	Message string `json:"message"`
	Time    string `json:"time"`
}

type PublicError struct {
	Code   int      `json:"code"`
	Errors []string `json:"errors"`
}
