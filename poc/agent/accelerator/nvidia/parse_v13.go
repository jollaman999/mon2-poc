package nvidia

import (
	"encoding/xml"
	"errors"
	"strconv"
	"strings"
)

// parseV13 는 v13 스키마의 nvidia-smi -q -x 출력을 원본과 같은 record 로 바꾼다.
func parseV13(buf []byte) ([]record, error) {
	var s smiV13
	if err := xml.Unmarshal(buf, &s); err != nil {
		return nil, err
	}

	var out []record

	for i := range s.Gpu {
		gpu := &s.Gpu[i]

		tags := map[string]string{
			"index": strconv.Itoa(i),
		}
		fields := make(map[string]any, 49)

		setTagIfUsed(tags, "pstate", gpu.PerformanceState)
		setTagIfUsed(tags, "name", gpu.ProductName)
		setTagIfUsed(tags, "arch", gpu.ProductArchitecture)
		setTagIfUsed(tags, "uuid", gpu.UUID)
		setTagIfUsed(tags, "compute_mode", gpu.ComputeMode)
		setTagIfUsed(tags, "pci_bus_id", gpu.Pci.PciBusID)

		setIfUsed("str", fields, "driver_version", s.DriverVersion)
		setIfUsed("str", fields, "cuda_version", s.CudaVersion)
		if _, ok := fields["driver_version"]; !ok {
			setIfUsed("str", fields, "driver_version", s.KmdVersion)
		}
		if _, ok := fields["cuda_version"]; !ok {
			setIfUsed("str", fields, "cuda_version", s.CudaUmdVersion)
		}
		setIfUsed("str", fields, "serial", gpu.Serial)
		setIfUsed("str", fields, "vbios_version", gpu.VbiosVersion)
		setIfUsed("str", fields, "display_active", gpu.DisplayActive)
		setIfUsed("str", fields, "display_mode", gpu.DisplayMode)
		setIfUsed("str", fields, "current_ecc", gpu.EccMode.CurrentEcc)
		setIfUsed("int", fields, "fan_speed", gpu.FanSpeed)
		setIfUsed("int", fields, "memory_total", gpu.FbMemoryUsage.Total)
		setIfUsed("int", fields, "memory_used", gpu.FbMemoryUsage.Used)
		setIfUsed("int", fields, "memory_free", gpu.FbMemoryUsage.Free)
		setIfUsed("int", fields, "memory_reserved", gpu.FbMemoryUsage.Reserved)
		setIfUsed("int", fields, "ecc_errors_volatile_dram_correctable", gpu.EccErrors.Volatile.DramCorrectable)
		setIfUsed("int", fields, "ecc_errors_volatile_dram_uncorrectable", gpu.EccErrors.Volatile.DramUncorrectable)
		setIfUsed("int", fields, "ecc_errors_volatile_sram_correctable", gpu.EccErrors.Volatile.SramCorrectable)
		setIfUsed("int", fields, "ecc_errors_volatile_sram_uncorrectable", gpu.EccErrors.Volatile.SramUncorrectable)
		setIfUsed("int", fields, "ecc_errors_volatile_sram_uncorrectable_parity", gpu.EccErrors.Volatile.SramUncorrectableParity)
		setIfUsed("int", fields, "ecc_errors_volatile_sram_uncorrectable_secded", gpu.EccErrors.Volatile.SramUncorrectableSecded)
		setIfUsed("int", fields, "ecc_errors_aggregate_dram_correctable", gpu.EccErrors.Aggregate.DramCorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_dram_uncorrectable", gpu.EccErrors.Aggregate.DramUncorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_correctable", gpu.EccErrors.Aggregate.SramCorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable", gpu.EccErrors.Aggregate.SramUncorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable_parity", gpu.EccErrors.Aggregate.SramUncorrectableParity)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable_secded", gpu.EccErrors.Aggregate.SramUncorrectableSecded)
		setIfUsed("str", fields, "ecc_errors_aggregate_sram_threshold_exceeded", gpu.EccErrors.Aggregate.SramThresholdExceeded)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable_l2", gpu.EccErrors.AggregateUncorrectableSramSources.SramL2)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable_microcontroller",
			gpu.EccErrors.AggregateUncorrectableSramSources.SramMicrocontroller)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable_other", gpu.EccErrors.AggregateUncorrectableSramSources.SramOther)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable_pcie", gpu.EccErrors.AggregateUncorrectableSramSources.SramPcie)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable_sm", gpu.EccErrors.AggregateUncorrectableSramSources.SramSm)
		setIfUsed("str", fields, "ecc_errors_channel_repair_pending", gpu.EccErrors.ChannelRepairPending)
		setIfUsed("str", fields, "ecc_errors_tpc_repair_pending", gpu.EccErrors.TpcRepairPending)
		setIfUsed("str", fields, "ecc_errors_unrepairable_memory", gpu.EccErrors.UnrepairableMemory)
		setIfUsed("int", fields, "retired_pages_multiple_single_bit", gpu.RetiredPages.MultipleSingleBitRetirement.RetiredCount)
		setIfUsed("int", fields, "retired_pages_double_bit", gpu.RetiredPages.DoubleBitRetirement.RetiredCount)
		setIfUsed("str", fields, "retired_pages_blacklist", gpu.RetiredPages.PendingBlacklist)
		setIfUsed("str", fields, "retired_pages_pending", gpu.RetiredPages.PendingRetirement)
		setIfUsed("int", fields, "remapped_rows_correctable", gpu.RemappedRows.RemappedRowCorr)
		setIfUsed("int", fields, "remapped_rows_uncorrectable", gpu.RemappedRows.RemappedRowUnc)
		setIfUsed("str", fields, "remapped_rows_pending", gpu.RemappedRows.RemappedRowPending)
		setIfUsed("str", fields, "remapped_rows_failure", gpu.RemappedRows.RemappedRowFailure)
		setIfUsed("int", fields, "temperature_gpu", gpu.Temperature.GpuTemp)
		setIfUsed("int", fields, "memory_temp", gpu.Temperature.MemoryTemp)
		setVgpuLicense(fields, gpu.VgpuSoftwareLicensedProduct.LicenseStatus)
		setMaxGPUTemperature(
			fields,
			gpu.Temperature.GpuTempMaxGpuThreshold,
			gpu.Temperature.GpuTemp,
			gpu.Temperature.GpuTempTlimit,
			gpu.Temperature.GpuTempMaxGpuTlimitThreshold,
		)
		setIfUsed("int", fields, "utilization_gpu", gpu.Utilization.GpuUtil)
		setIfUsed("int", fields, "utilization_memory", gpu.Utilization.MemoryUtil)
		setIfUsed("int", fields, "utilization_encoder", gpu.Utilization.EncoderUtil)
		setIfUsed("int", fields, "utilization_decoder", gpu.Utilization.DecoderUtil)
		setIfUsed("int", fields, "utilization_jpeg", gpu.Utilization.JpegUtil)
		setIfUsed("int", fields, "utilization_ofa", gpu.Utilization.OfaUtil)
		setIfUsed("int", fields, "pcie_link_gen_current", gpu.Pci.PciGpuLinkInfo.PcieGen.CurrentLinkGen)
		setIfUsed("int", fields, "pcie_link_width_current", gpu.Pci.PciGpuLinkInfo.LinkWidths.CurrentLinkWidth)
		setIfUsed("int", fields, "pcie_replay_counter", gpu.Pci.ReplayCounter)
		setClocksEventReasons(fields, map[string]string{
			"gpu_idle":                    gpu.ClocksEventReasons.ClocksEventReasonGpuIdle,
			"applications_clocks_setting": gpu.ClocksEventReasons.ClocksEventReasonApplicationsClocksSetting,
			"sw_power_cap":                gpu.ClocksEventReasons.ClocksEventReasonSwPowerCap,
			"hw_slowdown":                 gpu.ClocksEventReasons.ClocksEventReasonHwSlowdown,
			"sync_boost":                  gpu.ClocksEventReasons.ClocksEventReasonSyncBoost,
			"sw_thermal_slowdown":         gpu.ClocksEventReasons.ClocksEventReasonSwThermalSlowdown,
			"hw_thermal_slowdown":         gpu.ClocksEventReasons.ClocksEventReasonHwThermalSlowdown,
			"hw_power_brake_slowdown":     gpu.ClocksEventReasons.ClocksEventReasonHwPowerBrakeSlowdown,
			"display_clocks_setting":      gpu.ClocksEventReasons.ClocksEventReasonDisplayClocksSetting,
		})
		setIfUsed("int", fields, "encoder_stats_session_count", gpu.EncoderStats.SessionCount)
		setIfUsed("int", fields, "encoder_stats_average_fps", gpu.EncoderStats.AverageFps)
		setIfUsed("int", fields, "encoder_stats_average_latency", gpu.EncoderStats.AverageLatency)
		setIfUsed("int", fields, "fbc_stats_session_count", gpu.FbcStats.SessionCount)
		setIfUsed("int", fields, "fbc_stats_average_fps", gpu.FbcStats.AverageFps)
		setIfUsed("int", fields, "fbc_stats_average_latency", gpu.FbcStats.AverageLatency)
		setIfUsed("int", fields, "clocks_current_graphics", gpu.Clocks.GraphicsClock)
		setIfUsed("int", fields, "clocks_current_sm", gpu.Clocks.SmClock)
		setIfUsed("int", fields, "clocks_current_memory", gpu.Clocks.MemClock)
		setIfUsed("int", fields, "clocks_current_video", gpu.Clocks.VideoClock)
		setIfUsed("float", fields, "power_draw", gpu.PowerReadings.PowerDraw)
		setIfUsed("float", fields, "power_draw", gpu.PowerReadings.InstantPowerDraw)
		setIfUsed("float", fields, "power_limit", gpu.PowerReadings.PowerLimit)
		setIfUsed("float", fields, "power_draw", gpu.GpuPowerReadings.PowerDraw)
		setIfUsed("float", fields, "power_draw", gpu.GpuPowerReadings.InstantPowerDraw)
		setIfUsed("float", fields, "power_limit", gpu.GpuPowerReadings.CurrentPowerLimit)
		setIfUsed("float", fields, "power_limit", gpu.GpuPowerReadings.PowerLimit)
		setIfUsed("float", fields, "module_power_draw", gpu.ModulePowerReadings.PowerDraw)
		setIfUsed("float", fields, "module_power_draw", gpu.ModulePowerReadings.InstantPowerDraw)
		out = append(out, record{measGPU, tags, fields, i})

		for _, device := range gpu.MigDevices.MigDevice {
			tags := make(map[string]string, 8)
			setTagIfUsed(tags, "index", device.Index)
			setTagIfUsed(tags, "gpu_index", device.GpuInstanceID)
			setTagIfUsed(tags, "compute_index", device.ComputeInstanceID)
			setTagIfUsed(tags, "pstate", gpu.PerformanceState)
			setTagIfUsed(tags, "name", gpu.ProductName)
			setTagIfUsed(tags, "arch", gpu.ProductArchitecture)
			setTagIfUsed(tags, "uuid", gpu.UUID)
			setTagIfUsed(tags, "compute_mode", gpu.ComputeMode)

			fields := make(map[string]any, 8)
			setIfUsed("int", fields, "sram_uncorrectable", device.EccErrorCount.VolatileCount.SramUncorrectable)
			setIfUsed("int", fields, "memory_fb_total", device.FbMemoryUsage.Total)
			setIfUsed("int", fields, "memory_fb_reserved", device.FbMemoryUsage.Reserved)
			setIfUsed("int", fields, "memory_fb_used", device.FbMemoryUsage.Used)
			setIfUsed("int", fields, "memory_fb_free", device.FbMemoryUsage.Free)
			setIfUsed("int", fields, "memory_bar1_total", device.Bar1MemoryUsage.Total)
			setIfUsed("int", fields, "memory_bar1_used", device.Bar1MemoryUsage.Used)
			setIfUsed("int", fields, "memory_bar1_free", device.Bar1MemoryUsage.Free)

			out = append(out, record{measMIG, tags, fields, i})
		}

		for _, process := range gpu.Processes.ProcessInfo {
			tags := make(map[string]string, 2)
			setTagIfUsed(tags, "name", process.ProcessName)
			setTagIfUsed(tags, "type", process.Type)

			fields := make(map[string]any, 2)
			setIfUsed("int", fields, "pid", process.Pid)
			setIfUsed("int", fields, "used_memory", process.UsedMemory)

			out = append(out, record{measProcess, tags, fields, i})
		}
	}

	return out, nil
}

// setMaxGPUTemperature 는 GPU 최대 동작 온도(섭씨)를 넣는다.
//
// 일반 GPU 는 그 값을 절대 온도로 준다. 데이터센터 GPU(L40S, RTX PRO 6000 등)는 대신 T.Limit 을 주는데,
// nvidia-smi 는 이것을 "최대 동작 온도까지 남은 섭씨 여유" 로 정의한다. 그래서 최대값은 gpuTemp+tlimit 이고,
// maxTlimit 은 최대에 닿았다고 보는 여유(보통 0)다. 그런 GPU 는 절대 임계값도 목표 온도도 주지 않아서 이 방법뿐이다.
func setMaxGPUTemperature(fields map[string]any, maxThreshold, gpuTemp, tlimit, maxTlimit string) {
	if _, err := parseTemperature(maxThreshold); err == nil {
		setIfUsed("int", fields, "temperature_gpu_max", maxThreshold)
		return
	}

	current, err := parseTemperature(gpuTemp)
	if err != nil {
		return
	}
	margin, err := parseTemperature(tlimit)
	if err != nil {
		return
	}
	offset, err := parseTemperature(maxTlimit)
	if err != nil {
		return
	}

	fields["temperature_gpu_max"] = int64(current + margin - offset)
}

// parseTemperature 는 "29 C" 같은 온도 값에서 숫자를 뽑는다.
func parseTemperature(v string) (int, error) {
	fields := strings.Fields(v)
	if len(fields) < 1 {
		return 0, errors.New("empty temperature")
	}
	return strconv.Atoi(fields[0])
}
