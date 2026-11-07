import { Link } from "@tanstack/react-router"
import { Fragment } from "react"

import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { crumbs, toSplat } from "@/lib/paths"

/** Full virtual path as clickable segments. The last segment is the current page. */
export function PathCrumbs({ path }: { path: string }) {
  const items = crumbs(path)
  return (
    <Breadcrumb>
      <BreadcrumbList className="flex-wrap break-all">
        {items.map((c, i) => {
          const last = i === items.length - 1
          const label = c.path === "/" ? "전체" : c.name
          return (
            <Fragment key={c.path}>
              {i > 0 ? <BreadcrumbSeparator /> : null}
              <BreadcrumbItem>
                {last ? (
                  <BreadcrumbPage>{label}</BreadcrumbPage>
                ) : (
                  <BreadcrumbLink
                    render={c.path === "/" ? <Link to="/" /> : <Link to="/$" params={{ _splat: toSplat(c.path) }} />}
                  >
                    {label}
                  </BreadcrumbLink>
                )}
              </BreadcrumbItem>
            </Fragment>
          )
        })}
      </BreadcrumbList>
    </Breadcrumb>
  )
}
