import { LifeBuoy } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'

export function BackupOnlyBadge({ compact = false }: { compact?: boolean }) {
    const { t } = useTranslation()
    const label = t('channel.dialog.backupOnly')

    if (compact) {
        return (
            <Tooltip>
                <TooltipTrigger asChild>
                    <span className="inline-flex shrink-0 text-emerald-700 dark:text-emerald-400" aria-label={label}>
                        <LifeBuoy className="size-3.5" aria-hidden="true" />
                    </span>
                </TooltipTrigger>
                <TooltipContent>{label}</TooltipContent>
            </Tooltip>
        )
    }

    return (
        <Badge variant="outline" className="border-emerald-300 text-emerald-700 dark:border-emerald-800 dark:text-emerald-400">
            <LifeBuoy aria-hidden="true" />
            {label}
        </Badge>
    )
}
