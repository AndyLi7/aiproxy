import { describe, expect, it } from 'vitest'

import type { RuntimeMetricsResponse } from '@/types/runtime-metrics'
import { getChannelModelMetric } from './runtime-metrics'

const runtimeMetrics: RuntimeMetricsResponse = {
    models: {
        'test-model': {
            rpm: 100,
            tpm: 1000,
            rps: 10,
            tps: 100,
            requests: 200,
            errors: 20,
            error_rate: 0.1,
            banned_channels: 0,
            accessible_sets: [],
            accessible_groups: 0,
            channels: {},
        },
    },
    channels: {},
    channel_models: {
        '1': {
            'test-model': {
                rpm: 10,
                tpm: 100,
                rps: 1,
                tps: 10,
                requests: 20,
                errors: 1,
                error_rate: 0.05,
                banned: false,
            },
        },
    },
}

describe('getChannelModelMetric', () => {
    it('returns metrics scoped to the requested channel and model', () => {
        expect(getChannelModelMetric(runtimeMetrics, 1, 'test-model')).toMatchObject({
            rpm: 10,
            tpm: 100,
            error_rate: 0.05,
        })
    })

    it('does not fall back to aggregate model metrics for another channel', () => {
        expect(getChannelModelMetric(runtimeMetrics, 2, 'test-model')).toBeUndefined()
    })
})
