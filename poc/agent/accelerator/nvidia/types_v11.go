package nvidia

// smiV11 은 nvidia-smi -q -x 의 v10 · v11 스키마다. 원본(telegraf nvidia_smi)에서 옮겼다.
type smiV11 struct {
	GPU           []gpuV11 `xml:"gpu"`
	DriverVersion string   `xml:"driver_version"`
	CUDAVersion   string   `xml:"cuda_version"`
}

type gpuV11 struct {
	Clocks        clockStatsV11 `xml:"clocks"`
	ComputeMode   string        `xml:"compute_mode"`
	DisplayActive string        `xml:"display_active"`
	DisplayMode   string        `xml:"display_mode"`
	EccMode       eccModeV11    `xml:"ecc_mode"`
	EccErrors     eccErrorsV11  `xml:"ecc_errors"`
	VgpuLicense   struct {
		LicenseStatus string `xml:"license_status"`
	} `xml:"vgpu_software_licensed_product"`
	Encoder      encoderStatsV11  `xml:"encoder_stats"`
	FanSpeed     string           `xml:"fan_speed"`
	FBC          encoderStatsV11  `xml:"fbc_stats"`
	Memory       memoryStatsV11   `xml:"fb_memory_usage"`
	PCI          pciV11           `xml:"pci"`
	Power        powerReadingsV11 `xml:"power_readings"`
	ProdName     string           `xml:"product_name"`
	PState       string           `xml:"performance_state"`
	RemappedRows remappedRowsV11  `xml:"remapped_rows"`
	RetiredPages retiredPagesV11  `xml:"retired_pages"`
	Serial       string           `xml:"serial"`
	Temp         tempStatsV11     `xml:"temperature"`
	Utilization  utilizationV11   `xml:"utilization"`
	UUID         string           `xml:"uuid"`
	VbiosVersion string           `xml:"vbios_version"`
}

type eccModeV11 struct {
	CurrentEcc string `xml:"current_ecc"`
	PendingEcc string `xml:"pending_ecc"`
}

type memoryStatsV11 struct {
	Total    string `xml:"total"`
	Used     string `xml:"used"`
	Free     string `xml:"free"`
	Reserved string `xml:"reserved"`
}

type retiredPagesV11 struct {
	MultipleSingleBit struct {
		Count string `xml:"retired_count"`
	} `xml:"multiple_single_bit_retirement"`
	DoubleBit struct {
		Count string `xml:"retired_count"`
	} `xml:"double_bit_retirement"`
	PendingBlacklist  string `xml:"pending_blacklist"`
	PendingRetirement string `xml:"pending_retirement"`
}

type remappedRowsV11 struct {
	Correctable   string `xml:"remapped_row_corr"`
	Uncorrectable string `xml:"remapped_row_unc"`
	Pending       string `xml:"remapped_row_pending"`
	Failure       string `xml:"remapped_row_failure"`
}

type tempStatsV11 struct {
	GPUTemp                string `xml:"gpu_temp"`
	GPUTempMaxGPUThreshold string `xml:"gpu_temp_max_gpu_threshold"`
	MemoryTemp             string `xml:"memory_temp"`
}

// eccErrorsV11 은 v11 의 ECC 다. 옛 드라이버는 single_bit · double_bit 의 total 을,
// 새 드라이버(A10G, T4 표본)는 v12 와 같은 dram · sram 항목을 v11 DTD 그대로 낸다.
type eccErrorsV11 struct {
	Volatile  eccScopeV11 `xml:"volatile"`
	Aggregate eccScopeV11 `xml:"aggregate"`
}

type eccScopeV11 struct {
	SingleBit struct {
		Total string `xml:"total"`
	} `xml:"single_bit"`
	DoubleBit struct {
		Total string `xml:"total"`
	} `xml:"double_bit"`
	DramCorrectable   string `xml:"dram_correctable"`
	DramUncorrectable string `xml:"dram_uncorrectable"`
	SramCorrectable   string `xml:"sram_correctable"`
	SramUncorrectable string `xml:"sram_uncorrectable"`
}

type utilizationV11 struct {
	GPU     string `xml:"gpu_util"`
	Memory  string `xml:"memory_util"`
	Encoder string `xml:"encoder_util"`
	Decoder string `xml:"decoder_util"`
}

type powerReadingsV11 struct {
	PowerDraw         string `xml:"power_draw"`
	PowerLimit        string `xml:"power_limit"`
	CurrentPowerLimit string `xml:"current_power_limit"`
}

type pciV11 struct {
	LinkInfo struct {
		PCIEGen struct {
			CurrentLinkGen string `xml:"current_link_gen"`
		} `xml:"pcie_gen"`
		LinkWidth struct {
			CurrentLinkWidth string `xml:"current_link_width"`
		} `xml:"link_widths"`
	} `xml:"pci_gpu_link_info"`
	ReplayCounter string `xml:"replay_counter"`
}

type encoderStatsV11 struct {
	SessionCount   string `xml:"session_count"`
	AverageFPS     string `xml:"average_fps"`
	AverageLatency string `xml:"average_latency"`
}

type clockStatsV11 struct {
	Graphics string `xml:"graphics_clock"`
	SM       string `xml:"sm_clock"`
	Memory   string `xml:"mem_clock"`
	Video    string `xml:"video_clock"`
}
