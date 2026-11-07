import { Link } from "@tanstack/react-router"

export function NotFound() {
  return (
    <main className="grid min-h-svh place-items-center px-4 text-center">
      <div className="flex flex-col gap-2">
        <h1 className="text-lg font-semibold">페이지를 찾을 수 없습니다</h1>
        <Link to="/" className="text-sm text-primary underline underline-offset-4">
          저장소 목록으로 가기
        </Link>
      </div>
    </main>
  )
}
