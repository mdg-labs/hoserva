import type {ReactNode} from 'react';
import useBaseUrl from '@docusaurus/useBaseUrl';
import {cn} from '@site/src/lib/utils';
import type {AppEntry} from './types';

export function AppIcon({
  app,
  className,
}: {
  app: AppEntry;
  className?: string;
}): ReactNode {
  const src = useBaseUrl(`/apps/${app.id}/${app.icon ?? ''}`);
  return (
    <span
      aria-hidden="true"
      className={cn(
        'flex size-12 shrink-0 items-center justify-center overflow-hidden rounded-xl border bg-white p-2 text-lg font-semibold text-neutral-700',
        className,
      )}>
      {app.icon === undefined ? (
        app.title.charAt(0).toUpperCase()
      ) : (
        <img src={src} alt="" className="size-full object-contain" />
      )}
    </span>
  );
}
