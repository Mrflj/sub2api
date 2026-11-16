import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import OpsConcurrencyCard from '../OpsConcurrencyCard.vue'
import { opsAPI } from '@/api/admin/ops'

vi.mock('@/api/admin/ops', () => ({ opsAPI: {
  getConcurrencyStats: vi.fn().mockResolvedValue({ enabled: true, platform: {}, group: {}, account: {} }),
  getAccountAvailabilityStats: vi.fn().mockResolvedValue({ enabled: true }),
  getUserConcurrencyStats: vi.fn()
} }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)

describe('user concurrency activity', () => {
  it.each([0, -1])('shows active unlimited users with an unlimited denominator (%i)', async (limit) => {
    vi.mocked(opsAPI.getUserConcurrencyStats).mockResolvedValueOnce({ enabled: true, user: {
      '1': { user_id: 1, user_email: 'active@activation.local', username: 'active-user', current_in_use: 3, max_capacity: limit, load_percentage: 0, waiting_in_queue: 0 }
    } })
    const wrapper = mount(OpsConcurrencyCard, { props: { refreshToken: 0 } })
    await flushPromises()
    await wrapper.get('button[title="admin.ops.concurrency.switchToUser"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('active-user')
    expect(wrapper.text()).toContain('3/∞')
    expect(wrapper.text()).not.toContain('0%')
    expect(wrapper.find('[style*="width:"]').exists()).toBe(false)
  })

  it('keeps the finite capacity and percentage display', async () => {
    vi.mocked(opsAPI.getUserConcurrencyStats).mockResolvedValueOnce({ enabled: true, user: {
      '1': { user_id: 1, user_email: 'limited@example.com', username: '', current_in_use: 1, max_capacity: 2, load_percentage: 50, waiting_in_queue: 0 }
    } })
    const wrapper = mount(OpsConcurrencyCard, { props: { refreshToken: 0 } })
    await flushPromises()
    await wrapper.get('button[title="admin.ops.concurrency.switchToUser"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('1/2')
    expect(wrapper.text()).toContain('50%')
    expect(wrapper.find('[style*="width: 50%"]').exists()).toBe(true)
  })
})
