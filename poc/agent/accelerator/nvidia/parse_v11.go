package nvidia

import (
	"encoding/xml"
	"strconv"
)

// parseV11 는 v11 스키마의 nvidia-smi -q -x 출력을 원본과 같은 record 로 바꾼다.
func parseV11(buf []byte) ([]record, error) {
	var s smiV11
	if err := xml.Unmarshal(buf, &s); err != nil {
		return nil, err
	}

	var out []record
	for i := range s.GPU {
		gpu := &s.GPU[i]

		tags := map[string]string{
			"index": strconv.Itoa(i),
		}
		fields := make(map[string]any, 39)

		setTagIfUsed(tags, "pstate", gpu.PState)
		setTagIfUsed(tags, "name", gpu.ProdName)
		setTagIfUsed(tags, "uuid", gpu.UUID)
		setTagIfUsed(tags, "compute_mode", gpu.ComputeMode)

		setIfUsed("str", fields, "driver_version", s.DriverVersion)
		setIfUsed("str", fields, "cuda_version", s.CUDAVersion)
		setIfUsed("str", fields, "serial", gpu.Serial)
		setIfUsed("str", fields, "vbios_version", gpu.VbiosVersion)
		setIfUsed("str", fields, "display_active", gpu.DisplayActive)
		setIfUsed("str", fields, "display_mode", gpu.DisplayMode)
		setIfUsed("str", fields, "current_ecc", gpu.EccMode.CurrentEcc)
		setIfUsed("int", fields, "fan_speed", gpu.FanSpeed)
		setIfUsed("int", fields, "memory_total", gpu.Memory.Total)
		setIfUsed("int", fields, "memory_used", gpu.Memory.Used)
		setIfUsed("int", fields, "memory_free", gpu.Memory.Free)
		setIfUsed("int", fields, "memory_reserved", gpu.Memory.Reserved)
		setIfUsed("int", fields, "retired_pages_multiple_single_bit", gpu.RetiredPages.MultipleSingleBit.Count)
		setIfUsed("int", fields, "retired_pages_double_bit", gpu.RetiredPages.DoubleBit.Count)
		setIfUsed("str", fields, "retired_pages_blacklist", gpu.RetiredPages.PendingBlacklist)
		setIfUsed("str", fields, "retired_pages_pending", gpu.RetiredPages.PendingRetirement)
		setIfUsed("int", fields, "remapped_rows_correctable", gpu.RemappedRows.Correctable)
		setIfUsed("int", fields, "remapped_rows_uncorrectable", gpu.RemappedRows.Uncorrectable)
		setIfUsed("str", fields, "remapped_rows_pending", gpu.RemappedRows.Pending)
		setIfUsed("str", fields, "remapped_rows_failure", gpu.RemappedRows.Failure)
		setIfUsed("int", fields, "temperature_gpu", gpu.Temp.GPUTemp)
		setIfUsed("int", fields, "temperature_gpu_max", gpu.Temp.GPUTempMaxGPUThreshold)
		setIfUsed("int", fields, "memory_temp", gpu.Temp.MemoryTemp)
		setVgpuLicense(fields, gpu.VgpuLicense.LicenseStatus)
		// 옛 ECC 형식(single · double bit). single_bit 이 정정된 것, double_bit 이 정정 못 한 것이다
		setIfUsed("int", fields, "ecc_errors_volatile_single_bit", gpu.EccErrors.Volatile.SingleBit.Total)
		setIfUsed("int", fields, "ecc_errors_volatile_double_bit", gpu.EccErrors.Volatile.DoubleBit.Total)
		setIfUsed("int", fields, "ecc_errors_aggregate_single_bit", gpu.EccErrors.Aggregate.SingleBit.Total)
		setIfUsed("int", fields, "ecc_errors_aggregate_double_bit", gpu.EccErrors.Aggregate.DoubleBit.Total)
		setIfUsed("int", fields, "ecc_errors_volatile_dram_correctable", gpu.EccErrors.Volatile.DramCorrectable)
		setIfUsed("int", fields, "ecc_errors_volatile_dram_uncorrectable", gpu.EccErrors.Volatile.DramUncorrectable)
		setIfUsed("int", fields, "ecc_errors_volatile_sram_correctable", gpu.EccErrors.Volatile.SramCorrectable)
		setIfUsed("int", fields, "ecc_errors_volatile_sram_uncorrectable", gpu.EccErrors.Volatile.SramUncorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_dram_correctable", gpu.EccErrors.Aggregate.DramCorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_dram_uncorrectable", gpu.EccErrors.Aggregate.DramUncorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_correctable", gpu.EccErrors.Aggregate.SramCorrectable)
		setIfUsed("int", fields, "ecc_errors_aggregate_sram_uncorrectable", gpu.EccErrors.Aggregate.SramUncorrectable)
		setIfUsed("int", fields, "utilization_gpu", gpu.Utilization.GPU)
		setIfUsed("int", fields, "utilization_memory", gpu.Utilization.Memory)
		setIfUsed("int", fields, "utilization_encoder", gpu.Utilization.Encoder)
		setIfUsed("int", fields, "utilization_decoder", gpu.Utilization.Decoder)
		setIfUsed("int", fields, "pcie_link_gen_current", gpu.PCI.LinkInfo.PCIEGen.CurrentLinkGen)
		setIfUsed("int", fields, "pcie_link_width_current", gpu.PCI.LinkInfo.LinkWidth.CurrentLinkWidth)
		setIfUsed("int", fields, "pcie_replay_counter", gpu.PCI.ReplayCounter)
		setIfUsed("int", fields, "encoder_stats_session_count", gpu.Encoder.SessionCount)
		setIfUsed("int", fields, "encoder_stats_average_fps", gpu.Encoder.AverageFPS)
		setIfUsed("int", fields, "encoder_stats_average_latency", gpu.Encoder.AverageLatency)
		setIfUsed("int", fields, "fbc_stats_session_count", gpu.FBC.SessionCount)
		setIfUsed("int", fields, "fbc_stats_average_fps", gpu.FBC.AverageFPS)
		setIfUsed("int", fields, "fbc_stats_average_latency", gpu.FBC.AverageLatency)
		setIfUsed("int", fields, "clocks_current_graphics", gpu.Clocks.Graphics)
		setIfUsed("int", fields, "clocks_current_sm", gpu.Clocks.SM)
		setIfUsed("int", fields, "clocks_current_memory", gpu.Clocks.Memory)
		setIfUsed("int", fields, "clocks_current_video", gpu.Clocks.Video)

		setIfUsed("float", fields, "power_draw", gpu.Power.PowerDraw)
		setIfUsed("float", fields, "power_limit", gpu.Power.PowerLimit)
		out = append(out, record{measGPU, tags, fields, i})
	}

	return out, nil
}
