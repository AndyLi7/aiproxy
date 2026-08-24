// @vitest-environment jsdom

import { act, useState } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { MultiSelectCombobox } from './MultiSelectCombobox'

vi.mock('react-i18next', () => ({
    useTranslation: () => ({ t: (key: string) => key }),
}))

const reactTestEnvironment = globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
reactTestEnvironment.IS_REACT_ACT_ENVIRONMENT = true

describe('MultiSelectCombobox', () => {
    let container: HTMLDivElement
    let root: Root

    beforeEach(() => {
        container = document.createElement('div')
        document.body.appendChild(container)
        root = createRoot(container)
    })

    afterEach(async () => {
        await act(async () => root.unmount())
        container.remove()
    })

    it('does not submit its parent form when removing a selected item', async () => {
        const onSubmit = vi.fn()

        function Form() {
            const [selectedItems, setSelectedItems] = useState(['test-set'])

            return (
                <form
                    onSubmit={(event) => {
                        event.preventDefault()
                        onSubmit()
                    }}
                >
                    <MultiSelectCombobox
                        dropdownItems={[]}
                        selectedItems={selectedItems}
                        setSelectedItems={setSelectedItems}
                        handleFilteredDropdownItems={() => []}
                        handleDropdownItemDisplay={(item) => item}
                        handleSelectedItemDisplay={(item) => item}
                    />
                </form>
            )
        }

        await act(async () => root.render(<Form />))

        const removeButton = container.querySelector('button')
        expect(removeButton?.type).toBe('button')

        await act(async () => removeButton?.click())

        expect(onSubmit).not.toHaveBeenCalled()
        expect(container.textContent).not.toContain('test-set')
    })
})
